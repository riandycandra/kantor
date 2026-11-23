package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kana-consultant/kantor/backend/internal/dto"
	"github.com/kana-consultant/kantor/backend/internal/mail"
	platformmiddleware "github.com/kana-consultant/kantor/backend/internal/middleware"
	"github.com/kana-consultant/kantor/backend/internal/model"
	authrepo "github.com/kana-consultant/kantor/backend/internal/repository/auth"
	notificationsrepo "github.com/kana-consultant/kantor/backend/internal/repository/notifications"
	"github.com/kana-consultant/kantor/backend/internal/security"
	"github.com/kana-consultant/kantor/backend/internal/storage"
)

const (
	// documentMailSendTimeout bounds one synchronous send (dial + TLS + AUTH
	// + DATA). It stays below the server's 30 s WriteTimeout.
	documentMailSendTimeout = 20 * time.Second
	// documentMailStaleAfter releases the double-send guard for rows left in
	// queued/sending by a crashed process.
	documentMailStaleAfter    = 10 * time.Minute
	documentMailStaleMessage  = "Pengiriman terputus sebelum selesai"
	documentMailAdminNotice   = "admin:settings:manage"
	documentMailNoticeType    = "admin.document_mail.updated"
	documentMailNoticeRefType = "system_setting"
)

// DocumentMailStaleAfter / DocumentMailStaleMessage are shared with the HRIS
// document mailer, which releases the same guard before its own in-flight
// checks and sweeps rows of interrupted batches.
const (
	DocumentMailStaleAfter   = documentMailStaleAfter
	DocumentMailStaleMessage = documentMailStaleMessage
)

var (
	ErrDocumentMailNotReady               = errors.New("Email Dokumen belum aktif atau belum lengkap. Aktifkan dan lengkapi pengaturan di Admin > Settings.")
	ErrDocumentMailUsernameRequired       = errors.New("alamat Gmail wajib diisi saat Email Dokumen diaktifkan")
	ErrDocumentMailUsernameInvalid        = errors.New("alamat Gmail tidak valid: isi satu alamat email biasa, misalnya slip@perusahaan.co.id")
	ErrDocumentMailPasswordRequired       = errors.New("alamat Gmail atau port berubah: isi app password baru untuk akun tersebut")
	ErrDocumentMailForgeURLRequired       = errors.New("URL SMTP Forge wajib diisi saat Email Dokumen diaktifkan")
	ErrDocumentMailForgeURLInvalid        = errors.New("URL SMTP Forge tidak valid (gunakan http:// atau https://)")
	ErrDocumentMailForgeAPIKeyRequired    = errors.New("URL SMTP Forge berubah: isi API key baru untuk server tersebut")
	ErrDocumentMailForgeAccountInvalid    = errors.New("SMTP Account ID harus berupa UUID yang valid")
	ErrDocumentMailForgeFromEmailInvalid  = errors.New("email pengirim SMTP Forge tidak valid")
	ErrDocumentMailDeliveryNotFound       = errors.New("riwayat pengiriman email tidak ditemukan")
	ErrDocumentMailInFlight               = errors.New("pengiriman untuk dokumen ini masih berjalan")
	ErrDocumentMailInvalidRequest         = errors.New("permintaan pengiriman email dokumen tidak valid")
)

type documentMailSettingsRepository interface {
	GetDocumentMailRecord(ctx context.Context) (authrepo.DocumentMailSettingRecord, error)
	UpdateDocumentMail(ctx context.Context, updatedBy string, setting authrepo.DocumentMailSettingRecord) error
	GetUserByID(ctx context.Context, userID string) (model.User, error)
	ListUserIDsByPermission(ctx context.Context, permissionID string) ([]string, error)
	ResolveUserFullName(ctx context.Context, userID string) (string, error)
}

type documentMailDeliveriesRepository interface {
	Create(ctx context.Context, params notificationsrepo.CreateEmailDeliveryParams) (model.EmailDelivery, error)
	GetByID(ctx context.Context, id string) (model.EmailDelivery, error)
	MarkSending(ctx context.Context, id string) (model.EmailDelivery, error)
	SetExternalJobID(ctx context.Context, id string, externalJobID string) (model.EmailDelivery, error)
	MarkSent(ctx context.Context, id string, sentAt time.Time) (model.EmailDelivery, error)
	MarkFailed(ctx context.Context, id string, message string) (model.EmailDelivery, error)
	FailStaleInFlight(ctx context.Context, referenceType string, referenceID string, olderThan time.Duration, message string) (int64, error)
	FailStaleUnreferenced(ctx context.Context, kind string, olderThan time.Duration, message string) (int64, error)
}

type documentMailNotifier interface {
	CreateMany(ctx context.Context, params []notificationsrepo.CreateParams) error
}

// DocumentMailTransport is implemented by *mail.GmailSender.
type DocumentMailTransport interface {
	Send(ctx context.Context, creds mail.Credentials, msg mail.Message) error
	DevSMTPAddr() string
}

type presignStorage interface {
	storage.Storage
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
}

type forgeTestSession struct {
	baseURL string
	apiKey  string
	jobID   string
}

// DocumentMailService owns the tenant's 'document_mail' setting and is the
// single entry point for sending document email. Payslip and contract
// senders call Deliver (or Enqueue + Deliver with DeliveryID for batches);
// every attempt is recorded in email_deliveries.
type DocumentMailService struct {
	repo          documentMailSettingsRepository
	deliveries    documentMailDeliveriesRepository
	notifier      documentMailNotifier
	transport     DocumentMailTransport
	forge         *mail.SMTPForgeSender
	storage       storage.Storage
	encrypter     *security.Encrypter
	now           func() time.Time
	forgeSessions sync.Map
	// pdf is the read-only PDF conversion status shown next to the
	// settings (SetPDFStatus); it never holds a filesystem path.
	pdf dto.DocumentPDFStatus
}

// Document mail delivery modes reported in DocumentMailSettingResponse.
const (
	DocumentMailModeGmail      = "gmail"
	DocumentMailModeSMTPForge  = "smtp_forge"
	DocumentMailModeDevCapture = "dev_capture"
)

// DocumentMailRequest is one outgoing document email.
type DocumentMailRequest struct {
	Kind            string
	ReferenceType   *string
	ReferenceID     *string
	Recipient       string
	RecipientSource string
	Cc              []string
	ReplyTo         string
	Subject         string
	Text            string
	HTML            string
	Attachments     []mail.Attachment
	BatchID         *string
	RequestedBy     string
	// DeliveryID continues a queued row created earlier by Enqueue (batch
	// sends). The request must describe exactly what that row recorded
	// (kind, reference, recipient + source, cc, subject, attachments), so the
	// log always shows what was actually sent. Empty creates a new row;
	// a retry ("Kirim ulang") of a failed row is a new row too, so every
	// attempt keeps its own recipient and digests.
	DeliveryID string
}

func NewDocumentMailService(
	repo documentMailSettingsRepository,
	deliveries documentMailDeliveriesRepository,
	notifier documentMailNotifier,
	transport DocumentMailTransport,
	encrypter *security.Encrypter,
) *DocumentMailService {
	return &DocumentMailService{
		repo:       repo,
		deliveries: deliveries,
		notifier:   notifier,
		transport:  transport,
		forge:      mail.NewSMTPForgeSender(nil),
		encrypter:  encrypter,
		now:        time.Now,
		pdf:        dto.DocumentPDFStatus{Enabled: false, Source: "not_found"},
	}
}

// SetStorage attaches the storage provider used for temporary presigned S3
// attachment URLs when sending via SMTP Forge.
func (s *DocumentMailService) SetStorage(store storage.Storage) {
	s.storage = store
}

// SetForgeSender overrides the SMTP Forge HTTP sender (used in unit tests).
func (s *DocumentMailService) SetForgeSender(forge *mail.SMTPForgeSender) {
	s.forge = forge
}

// SetPDFStatus records whether the document engine converts to PDF and how
// the LibreOffice binary was resolved (config.SofficeSource*). It is shown
// read-only on the settings page.
func (s *DocumentMailService) SetPDFStatus(enabled bool, source string) {
	s.pdf = dto.DocumentPDFStatus{Enabled: enabled, Source: source}
}

func (s *DocumentMailService) devSMTPAddr() string {
	if s.transport == nil {
		return ""
	}
	return s.transport.DevSMTPAddr()
}

// effectiveDevSMTPAddr returns the local Mailpit capture address only when
// Gmail is the active provider. Selecting smtp_forge bypasses Mailpit
// automatically so admins can test/send directly through SMTP Forge.
func (s *DocumentMailService) effectiveDevSMTPAddr(setting authrepo.DocumentMailSettingRecord) string {
	if setting.Provider == authrepo.DocumentMailProviderSMTPForge {
		return ""
	}
	return s.devSMTPAddr()
}

func (s *DocumentMailService) toResponse(setting authrepo.DocumentMailSettingRecord) dto.DocumentMailSettingResponse {
	normalized := authrepo.NormalizeDocumentMailSettingRecord(setting)
	response := dto.DocumentMailSettingResponse{
		Enabled:            normalized.Enabled,
		Provider:           normalized.Provider,
		SMTPHost:           mail.GmailHost,
		SMTPUsername:       normalized.SMTPUsername,
		SMTPPort:           normalized.SMTPPort,
		SenderName:         normalized.SenderName,
		HasSMTPPassword:    normalized.HasPassword(),
		SMTPForgeBaseURL:   normalized.SMTPForgeBaseURL,
		HasSMTPForgeAPIKey: normalized.HasSMTPForgeAPIKey(),
		SMTPForgeAccountID: normalized.SMTPForgeAccountID,
		SMTPForgeFromEmail: normalized.SMTPForgeFromEmail,
		Ready:              s.isReady(normalized),
	}
	if normalized.Provider == authrepo.DocumentMailProviderSMTPForge {
		response.Delivery = dto.DocumentMailDeliveryStatus{Mode: DocumentMailModeSMTPForge}
	} else {
		response.Delivery = dto.DocumentMailDeliveryStatus{Mode: DocumentMailModeGmail}
		if addr := s.effectiveDevSMTPAddr(normalized); addr != "" {
			devAddr, captureAddr := addr, addr
			response.DevSMTPAddr = &devAddr
			response.Delivery = dto.DocumentMailDeliveryStatus{Mode: DocumentMailModeDevCapture, CaptureAddr: &captureAddr}
		}
	}
	response.PDF = s.pdf
	return response
}

func (s *DocumentMailService) isReady(setting authrepo.DocumentMailSettingRecord) bool {
	normalized := authrepo.NormalizeDocumentMailSettingRecord(setting)
	if !normalized.Enabled {
		return false
	}
	if normalized.Provider == authrepo.DocumentMailProviderSMTPForge {
		if normalized.SMTPForgeBaseURL == "" || !normalized.HasSMTPForgeAPIKey() {
			return false
		}
		if _, err := mail.NormalizeSMTPForgeAPIBase(normalized.SMTPForgeBaseURL); err != nil {
			return false
		}
		if normalized.SMTPForgeFromEmail != "" && mail.ValidateAddress(normalized.SMTPForgeFromEmail) != nil {
			return false
		}
		if normalized.SMTPForgeAccountID != "" {
			if _, err := uuid.Parse(normalized.SMTPForgeAccountID); err != nil {
				return false
			}
		}
		return true
	}
	return normalized.SMTPUsername != "" &&
		mail.ValidateAddress(normalized.SMTPUsername) == nil &&
		(normalized.HasPassword() || s.effectiveDevSMTPAddr(normalized) != "")
}

// GetSettings returns the public view (has_smtp_password only).
func (s *DocumentMailService) GetSettings(ctx context.Context) (dto.DocumentMailSettingResponse, error) {
	setting, err := s.repo.GetDocumentMailRecord(ctx)
	if err != nil {
		return dto.DocumentMailSettingResponse{}, err
	}
	return s.toResponse(setting), nil
}

// Ready reports whether document email can be sent right now.
func (s *DocumentMailService) Ready(ctx context.Context) (bool, error) {
	setting, err := s.repo.GetDocumentMailRecord(ctx)
	if err != nil {
		return false, err
	}
	return s.isReady(setting), nil
}

// UpdateSettings applies the admin's edit and returns the new public view plus
// the names of the fields that changed (for the audit entry and the admin
// notice). The app password is cleared whenever the username or port changes,
// and such a change is refused unless a new password comes in the same
// request; clear_smtp_password does not waive that rule. Removing the account
// (blank username) is the exception: there is no account left to need a
// password, so the stored one is simply dropped.
func (s *DocumentMailService) UpdateSettings(ctx context.Context, actorID string, input dto.UpdateDocumentMailRequest) (dto.DocumentMailSettingResponse, []string, error) {
	existing, err := s.repo.GetDocumentMailRecord(ctx)
	if err != nil {
		return dto.DocumentMailSettingResponse{}, nil, err
	}

	updated := existing
	updated.Enabled = input.Enabled
	if strings.TrimSpace(input.Provider) != "" {
		updated.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	}
	if updated.Provider != authrepo.DocumentMailProviderSMTPForge {
		updated.Provider = authrepo.DocumentMailProviderGmail
	}
	updated.SenderName = sanitizeSenderName(input.SenderName)

	if updated.Provider == authrepo.DocumentMailProviderGmail {
		updated.SMTPUsername = strings.ToLower(strings.TrimSpace(input.SMTPUsername))
		if input.SMTPPort == 587 || input.SMTPPort == 465 {
			updated.SMTPPort = input.SMTPPort
		} else if updated.SMTPPort != 587 && updated.SMTPPort != 465 {
			updated.SMTPPort = authrepo.DefaultDocumentMailPort
		}

		if updated.Enabled && updated.SMTPUsername == "" {
			return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailUsernameRequired
		}
		if updated.SMTPUsername != "" && mail.ValidateAddress(updated.SMTPUsername) != nil {
			return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailUsernameInvalid
		}

		newPassword := ""
		if input.SMTPPassword != nil {
			newPassword = strings.Join(strings.Fields(*input.SMTPPassword), "")
		}

		clearPassword := input.ClearSMTPPassword
		accountChanged := updated.SMTPUsername != existing.SMTPUsername || updated.SMTPPort != existing.SMTPPort
		switch {
		case updated.SMTPUsername == "":
			clearPassword = true
			newPassword = ""
		case accountChanged && existing.HasPassword():
			if newPassword == "" {
				return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailPasswordRequired
			}
			updated.SMTPPasswordEncrypted = ""
			clearPassword = false
		}

		switch {
		case clearPassword:
			updated.SMTPPasswordEncrypted = ""
		case newPassword != "":
			if s.encrypter == nil {
				return dto.DocumentMailSettingResponse{}, nil, errors.New("document mail encrypter is not configured")
			}
			ciphertext, err := s.encrypter.EncryptString(newPassword)
			if err != nil {
				return dto.DocumentMailSettingResponse{}, nil, err
			}
			updated.SMTPPasswordEncrypted = ciphertext
		}
	} else {
		updated.SMTPForgeBaseURL = strings.TrimRight(strings.TrimSpace(input.SMTPForgeBaseURL), "/")
		updated.SMTPForgeAccountID = strings.TrimSpace(input.SMTPForgeAccountID)
		updated.SMTPForgeFromEmail = strings.ToLower(strings.TrimSpace(input.SMTPForgeFromEmail))

		if updated.Enabled && updated.SMTPForgeBaseURL == "" {
			return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailForgeURLRequired
		}
		if updated.SMTPForgeBaseURL != "" {
			if _, err := mail.NormalizeSMTPForgeAPIBase(updated.SMTPForgeBaseURL); err != nil {
				return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailForgeURLInvalid
			}
		}
		if updated.SMTPForgeAccountID != "" {
			if _, err := uuid.Parse(updated.SMTPForgeAccountID); err != nil {
				return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailForgeAccountInvalid
			}
		}
		if updated.SMTPForgeFromEmail != "" && mail.ValidateAddress(updated.SMTPForgeFromEmail) != nil {
			return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailForgeFromEmailInvalid
		}

		newAPIKey := ""
		if input.SMTPForgeAPIKey != nil {
			newAPIKey = strings.TrimSpace(*input.SMTPForgeAPIKey)
		}

		clearAPIKey := input.ClearSMTPForgeAPIKey
		urlChanged := updated.SMTPForgeBaseURL != existing.SMTPForgeBaseURL
		switch {
		case updated.SMTPForgeBaseURL == "":
			clearAPIKey = true
			newAPIKey = ""
		case urlChanged && existing.HasSMTPForgeAPIKey():
			if newAPIKey == "" {
				return dto.DocumentMailSettingResponse{}, nil, ErrDocumentMailForgeAPIKeyRequired
			}
			updated.SMTPForgeAPIKeyEncrypted = ""
			clearAPIKey = false
		}

		switch {
		case clearAPIKey:
			updated.SMTPForgeAPIKeyEncrypted = ""
		case newAPIKey != "":
			if s.encrypter == nil {
				return dto.DocumentMailSettingResponse{}, nil, errors.New("document mail encrypter is not configured")
			}
			ciphertext, err := s.encrypter.EncryptString(newAPIKey)
			if err != nil {
				return dto.DocumentMailSettingResponse{}, nil, err
			}
			updated.SMTPForgeAPIKeyEncrypted = ciphertext
		}
	}

	updated = authrepo.NormalizeDocumentMailSettingRecord(updated)
	changed := documentMailChangedFields(existing, updated)

	if err := s.repo.UpdateDocumentMail(ctx, actorID, updated); err != nil {
		return dto.DocumentMailSettingResponse{}, nil, err
	}

	if len(changed) > 0 {
		s.notifyAdmins(ctx, actorID, changed)
	}

	return s.toResponse(updated), changed, nil
}

func documentMailChangedFields(before authrepo.DocumentMailSettingRecord, after authrepo.DocumentMailSettingRecord) []string {
	changed := make([]string, 0, 10)
	if before.Enabled != after.Enabled {
		changed = append(changed, "enabled")
	}
	if before.Provider != after.Provider {
		changed = append(changed, "provider")
	}
	if before.SMTPUsername != after.SMTPUsername {
		changed = append(changed, "smtp_username")
	}
	if before.SMTPPasswordEncrypted != after.SMTPPasswordEncrypted {
		changed = append(changed, "smtp_password")
	}
	if before.SMTPPort != after.SMTPPort {
		changed = append(changed, "smtp_port")
	}
	if before.SenderName != after.SenderName {
		changed = append(changed, "sender_name")
	}
	if before.SMTPForgeBaseURL != after.SMTPForgeBaseURL {
		changed = append(changed, "smtp_forge_base_url")
	}
	if before.SMTPForgeAPIKeyEncrypted != after.SMTPForgeAPIKeyEncrypted {
		changed = append(changed, "smtp_forge_api_key")
	}
	if before.SMTPForgeAccountID != after.SMTPForgeAccountID {
		changed = append(changed, "smtp_forge_account_id")
	}
	if before.SMTPForgeFromEmail != after.SMTPForgeFromEmail {
		changed = append(changed, "smtp_forge_from_email")
	}
	return changed
}

// notifyAdmins sends an in-app notice to every user who can manage settings
// (including the actor, so a hijacked session is visible to the real owner).
// Best effort: the settings are already saved.
func (s *DocumentMailService) notifyAdmins(ctx context.Context, actorID string, changed []string) {
	if s.notifier == nil {
		return
	}
	recipients, err := s.repo.ListUserIDsByPermission(ctx, documentMailAdminNotice)
	if err != nil {
		slog.WarnContext(ctx, "document mail: list admin recipients failed", "error", err)
		return
	}
	if len(recipients) == 0 {
		return
	}

	actorName := "Seorang admin"
	if name, err := s.repo.ResolveUserFullName(ctx, actorID); err == nil && strings.TrimSpace(name) != "" {
		actorName = strings.TrimSpace(name)
	}
	message := fmt.Sprintf("%s mengubah pengaturan Email Dokumen (Gmail): %s.", actorName, strings.Join(changed, ", "))
	referenceType := documentMailNoticeRefType

	params := make([]notificationsrepo.CreateParams, 0, len(recipients))
	seen := make(map[string]bool, len(recipients))
	for _, userID := range recipients {
		if userID == "" || seen[userID] {
			continue
		}
		seen[userID] = true
		params = append(params, notificationsrepo.CreateParams{
			UserID:        userID,
			Type:          documentMailNoticeType,
			Title:         "Pengaturan Email Dokumen diubah",
			Message:       message,
			ReferenceType: &referenceType,
		})
	}
	if err := s.notifier.CreateMany(ctx, params); err != nil {
		slog.WarnContext(ctx, "document mail: admin notification failed", "error", err)
	}
}

func (s *DocumentMailService) gmailCredentials(setting authrepo.DocumentMailSettingRecord, overridePassword *string) (mail.Credentials, error) {
	password := ""
	if overridePassword != nil && strings.TrimSpace(*overridePassword) != "" {
		password = strings.Join(strings.Fields(*overridePassword), "")
	} else if setting.HasPassword() {
		if s.encrypter == nil {
			return mail.Credentials{}, errors.New("document mail encrypter is not configured")
		}
		var err error
		password, err = s.encrypter.DecryptString(setting.SMTPPasswordEncrypted)
		if err != nil {
			return mail.Credentials{}, fmt.Errorf("decrypt document mail password: %w", err)
		}
	}

	return mail.Credentials{
		Username:   setting.SMTPUsername,
		Password:   password,
		Port:       setting.SMTPPort,
		SenderName: setting.SenderName,
	}, nil
}

func (s *DocumentMailService) forgeCredentials(setting authrepo.DocumentMailSettingRecord, overrideAPIKey *string) (mail.SMTPForgeCredentials, error) {
	apiKey := ""
	if overrideAPIKey != nil && strings.TrimSpace(*overrideAPIKey) != "" {
		apiKey = strings.TrimSpace(*overrideAPIKey)
	} else if setting.HasSMTPForgeAPIKey() {
		if s.encrypter == nil {
			return mail.SMTPForgeCredentials{}, errors.New("document mail encrypter is not configured")
		}
		var err error
		apiKey, err = s.encrypter.DecryptString(setting.SMTPForgeAPIKeyEncrypted)
		if err != nil {
			return mail.SMTPForgeCredentials{}, fmt.Errorf("decrypt smtp forge api key: %w", err)
		}
	}

	return mail.SMTPForgeCredentials{
		BaseURL:    setting.SMTPForgeBaseURL,
		APIKey:     apiKey,
		AccountID:  setting.SMTPForgeAccountID,
		FromEmail:  setting.SMTPForgeFromEmail,
		SenderName: setting.SenderName,
	}, nil
}

func validateDocumentMailRequest(req DocumentMailRequest) error {
	switch req.Kind {
	case model.EmailDeliveryKindContract, model.EmailDeliveryKindPayslip, model.EmailDeliveryKindTest, model.EmailDeliveryKindLegal:
	default:
		return fmt.Errorf("%w: kind %q", ErrDocumentMailInvalidRequest, req.Kind)
	}
	switch req.RecipientSource {
	case model.EmailRecipientSourceLogin, model.EmailRecipientSourceEmployee, model.EmailRecipientSourcePersonal, model.EmailRecipientSourceSelf, model.EmailRecipientSourceParty:
	default:
		return fmt.Errorf("%w: recipient source %q", ErrDocumentMailInvalidRequest, req.RecipientSource)
	}
	if (req.ReferenceType == nil) != (req.ReferenceID == nil) {
		return fmt.Errorf("%w: reference type and id go together", ErrDocumentMailInvalidRequest)
	}
	if err := mail.ValidateAddress(req.Recipient); err != nil {
		return fmt.Errorf("%w: recipient: %v", ErrDocumentMailInvalidRequest, err)
	}
	for _, cc := range req.Cc {
		if err := mail.ValidateAddress(cc); err != nil {
			return fmt.Errorf("%w: cc: %v", ErrDocumentMailInvalidRequest, err)
		}
	}
	if strings.TrimSpace(req.Subject) == "" {
		return fmt.Errorf("%w: subject is required", ErrDocumentMailInvalidRequest)
	}
	return nil
}

func attachmentDigests(attachments []mail.Attachment) ([]string, []string) {
	names := make([]string, 0, len(attachments))
	digests := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		sum := sha256.Sum256(attachment.Data)
		names = append(names, strings.TrimSpace(attachment.Filename))
		digests = append(digests, hex.EncodeToString(sum[:]))
	}
	return names, digests
}

func (s *DocumentMailService) createParams(req DocumentMailRequest, status string) notificationsrepo.CreateEmailDeliveryParams {
	names, digests := attachmentDigests(req.Attachments)
	var requestedBy *string
	if strings.TrimSpace(req.RequestedBy) != "" {
		value := strings.TrimSpace(req.RequestedBy)
		requestedBy = &value
	}
	return notificationsrepo.CreateEmailDeliveryParams{
		Kind:             req.Kind,
		ReferenceType:    req.ReferenceType,
		ReferenceID:      req.ReferenceID,
		Recipient:        strings.ToLower(strings.TrimSpace(req.Recipient)),
		RecipientSource:  req.RecipientSource,
		Cc:               req.Cc,
		Subject:          strings.TrimSpace(req.Subject),
		AttachmentNames:  names,
		AttachmentSHA256: digests,
		Status:           status,
		BatchID:          req.BatchID,
		RequestedBy:      requestedBy,
	}
}

// Enqueue records a queued delivery (batch sends). The batch worker later
// calls Deliver with DeliveryID set to the returned row.
func (s *DocumentMailService) Enqueue(ctx context.Context, req DocumentMailRequest) (model.EmailDelivery, error) {
	if err := validateDocumentMailRequest(req); err != nil {
		return model.EmailDelivery{}, err
	}
	s.releaseStale(ctx, req)
	delivery, err := s.deliveries.Create(ctx, s.createParams(req, model.EmailDeliveryStatusQueued))
	if errors.Is(err, notificationsrepo.ErrEmailDeliveryInFlight) {
		return model.EmailDelivery{}, ErrDocumentMailInFlight
	}
	return delivery, err
}

func (s *DocumentMailService) releaseStale(ctx context.Context, req DocumentMailRequest) {
	var err error
	if req.ReferenceType == nil || req.ReferenceID == nil {
		// Rows without a document (test emails) never block a send, but a
		// crashed attempt must not stay 'sending' in the log forever.
		_, err = s.deliveries.FailStaleUnreferenced(ctx, req.Kind, documentMailStaleAfter, documentMailStaleMessage)
	} else {
		_, err = s.deliveries.FailStaleInFlight(ctx, *req.ReferenceType, *req.ReferenceID, documentMailStaleAfter, documentMailStaleMessage)
	}
	if err != nil {
		slog.WarnContext(ctx, "document mail: releasing stale deliveries failed", "error", err)
	}
}

// matchesQueued reports whether req describes exactly what the queued row
// recorded, so continuing it cannot log an address or digest that was never
// sent.
func matchesQueued(row model.EmailDelivery, params notificationsrepo.CreateEmailDeliveryParams) bool {
	return row.Kind == params.Kind &&
		equalOptional(row.ReferenceType, params.ReferenceType) &&
		equalOptional(row.ReferenceID, params.ReferenceID) &&
		row.Recipient == params.Recipient &&
		row.RecipientSource == params.RecipientSource &&
		equalStrings(row.Cc, params.Cc) &&
		row.Subject == params.Subject &&
		equalStrings(row.AttachmentNames, params.AttachmentNames) &&
		equalStrings(row.AttachmentSHA256, params.AttachmentSHA256)
}

func equalOptional(a *string, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return strings.EqualFold(strings.TrimSpace(*a), strings.TrimSpace(*b))
}

func equalStrings(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// startQueued moves a queued row (from Enqueue) to sending after checking
// that req matches it.
func (s *DocumentMailService) startQueued(ctx context.Context, id string, req DocumentMailRequest) (model.EmailDelivery, error) {
	row, err := s.deliveries.GetByID(ctx, id)
	if errors.Is(err, notificationsrepo.ErrEmailDeliveryNotFound) {
		return model.EmailDelivery{}, fmt.Errorf("%w: delivery %s not found", ErrDocumentMailInvalidRequest, id)
	}
	if err != nil {
		return model.EmailDelivery{}, err
	}
	switch row.Status {
	case model.EmailDeliveryStatusQueued:
	case model.EmailDeliveryStatusSending:
		return model.EmailDelivery{}, ErrDocumentMailInFlight
	default:
		return model.EmailDelivery{}, fmt.Errorf("%w: delivery %s is %s; a retry is a new delivery", ErrDocumentMailInvalidRequest, id, row.Status)
	}
	if !matchesQueued(row, s.createParams(req, model.EmailDeliveryStatusQueued)) {
		return model.EmailDelivery{}, fmt.Errorf("%w: request does not match queued delivery %s", ErrDocumentMailInvalidRequest, id)
	}
	delivery, err := s.deliveries.MarkSending(ctx, id)
	if errors.Is(err, notificationsrepo.ErrEmailDeliveryNotFound) {
		// Another worker picked the row up between the read and the update.
		return model.EmailDelivery{}, ErrDocumentMailInFlight
	}
	return delivery, err
}

// Deliver sends one document email synchronously (20 s budget) and records
// the attempt. When the send fails, the returned delivery is the failed
// row and the error is a *mail.SendError carrying the fixed category.
// ErrDocumentMailNotReady / ErrDocumentMailInFlight / ErrDocumentMailInvalidRequest
// mean nothing was sent and (for the first two) no row was written.
func (s *DocumentMailService) Deliver(ctx context.Context, req DocumentMailRequest) (model.EmailDelivery, error) {
	setting, err := s.repo.GetDocumentMailRecord(ctx)
	if err != nil {
		return model.EmailDelivery{}, err
	}
	if !s.isReady(setting) {
		return model.EmailDelivery{}, ErrDocumentMailNotReady
	}
	return s.deliverWithSetting(ctx, setting, nil, nil, req)
}

func (s *DocumentMailService) deliverWithSetting(
	ctx context.Context,
	setting authrepo.DocumentMailSettingRecord,
	overridePassword *string,
	overrideAPIKey *string,
	req DocumentMailRequest,
) (model.EmailDelivery, error) {
	if err := validateDocumentMailRequest(req); err != nil {
		return model.EmailDelivery{}, err
	}

	normalized := authrepo.NormalizeDocumentMailSettingRecord(setting)
	var gmailCreds mail.Credentials
	var forgeCreds mail.SMTPForgeCredentials
	var err error
	if normalized.Provider == authrepo.DocumentMailProviderSMTPForge {
		forgeCreds, err = s.forgeCredentials(normalized, overrideAPIKey)
	} else {
		gmailCreds, err = s.gmailCredentials(normalized, overridePassword)
	}
	if err != nil {
		return model.EmailDelivery{}, err
	}

	// From here on the attempt is bound to its delivery row, not to the
	// caller: a client disconnect must neither cut an SMTP transaction that
	// may already have been accepted nor leave the row in 'sending'.
	// WithoutCancel keeps the context values (tenant DB connection); each
	// repository call still has its own query timeout and the send has
	// documentMailSendTimeout.
	ctx = context.WithoutCancel(ctx)

	var delivery model.EmailDelivery
	if id := strings.TrimSpace(req.DeliveryID); id != "" {
		delivery, err = s.startQueued(ctx, id, req)
	} else {
		s.releaseStale(ctx, req)
		delivery, err = s.deliveries.Create(ctx, s.createParams(req, model.EmailDeliveryStatusSending))
	}
	if errors.Is(err, notificationsrepo.ErrEmailDeliveryInFlight) {
		return model.EmailDelivery{}, ErrDocumentMailInFlight
	}
	if err != nil {
		return model.EmailDelivery{}, err
	}

	message := mail.Message{
		To:          strings.TrimSpace(req.Recipient),
		Cc:          req.Cc,
		ReplyTo:     strings.TrimSpace(req.ReplyTo),
		Subject:     req.Subject,
		Text:        req.Text,
		HTML:        req.HTML,
		Attachments: req.Attachments,
	}

	if normalized.Provider == authrepo.DocumentMailProviderSMTPForge {
		return s.deliverViaForge(ctx, delivery, forgeCreds, message, req.Kind)
	}

	sendCtx, cancel := context.WithTimeout(ctx, documentMailSendTimeout)
	sendErr := s.transport.Send(sendCtx, gmailCreds, message)
	cancel()

	if sendErr != nil {
		return s.recordDeliveryFailure(ctx, delivery, req.Kind, sendErr)
	}

	sent, err := s.deliveries.MarkSent(ctx, delivery.ID, s.now())
	if err != nil {
		// The mail left; only the bookkeeping failed. Report it as sent.
		slog.ErrorContext(ctx, "document mail: recording sent delivery failed", "delivery_id", delivery.ID, "error", err)
		delivery.Status = model.EmailDeliveryStatusSent
		return delivery, nil
	}
	return sent, nil
}

func (s *DocumentMailService) deliverViaForge(
	ctx context.Context,
	delivery model.EmailDelivery,
	creds mail.SMTPForgeCredentials,
	message mail.Message,
	kind string,
) (model.EmailDelivery, error) {
	if s.forge == nil {
		s.forge = mail.NewSMTPForgeSender(nil)
	}

	forgeAttachments, tempKeys, err := s.uploadForgeAttachments(ctx, delivery.ID, message.Attachments)
	if err != nil {
		return s.recordDeliveryFailure(ctx, delivery, kind, err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, documentMailSendTimeout)
	jobID, sendErr := s.forge.Send(sendCtx, creds, message, forgeAttachments)
	cancel()

	if sendErr != nil {
		s.cleanupTempS3Keys(ctx, tempKeys)
		return s.recordDeliveryFailure(ctx, delivery, kind, sendErr)
	}

	if updated, err := s.deliveries.SetExternalJobID(ctx, delivery.ID, jobID); err == nil {
		delivery = updated
	} else {
		delivery.ExternalJobID = &jobID
	}

	s.forgeSessions.Store(delivery.ID, forgeTestSession{
		baseURL: creds.BaseURL,
		apiKey:  creds.APIKey,
		jobID:   jobID,
	})

	detached := platformmiddleware.DetachTenantContext(ctx)
	go s.monitorForgeJob(detached, delivery.ID, creds.BaseURL, creds.APIKey, jobID, tempKeys)
	return delivery, nil
}

func (s *DocumentMailService) uploadForgeAttachments(
	ctx context.Context,
	deliveryID string,
	attachments []mail.Attachment,
) ([]mail.SMTPForgeAttachment, []string, error) {
	if len(attachments) == 0 {
		return nil, nil, nil
	}
	presigner, ok := s.storage.(presignStorage)
	if !ok || s.storage == nil {
		return nil, nil, &mail.SendError{
			Category: mail.CategoryConfig,
			Detail:   "SMTP Forge membutuhkan STORAGE_DRIVER=s3 untuk mengirim lampiran",
		}
	}

	result := make([]mail.SMTPForgeAttachment, 0, len(attachments))
	keys := make([]string, 0, len(attachments))
	for idx, att := range attachments {
		name := filepath.Base(strings.TrimSpace(att.Filename))
		if name == "" || name == "." {
			name = fmt.Sprintf("attachment-%d.bin", idx+1)
		}
		key := fmt.Sprintf("tmp/document-mail/%s/%d-%s", deliveryID, idx, name)
		contentType := strings.TrimSpace(att.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if err := presigner.Put(ctx, key, bytes.NewReader(att.Data), int64(len(att.Data)), contentType); err != nil {
			s.cleanupTempS3Keys(ctx, keys)
			return nil, nil, mail.NewSendError(mail.CategoryConnect, 0, "s3", "", err)
		}
		keys = append(keys, key)

		signedURL, err := presigner.PresignGet(ctx, key, 15*time.Minute)
		if err != nil {
			s.cleanupTempS3Keys(ctx, keys)
			return nil, nil, mail.NewSendError(mail.CategoryConfig, 0, "", "gagal membuat URL sementara S3", err)
		}
		result = append(result, mail.SMTPForgeAttachment{
			Filename: name,
			URL:      signedURL,
		})
	}
	return result, keys, nil
}

func (s *DocumentMailService) cleanupTempS3Keys(ctx context.Context, keys []string) {
	if s.storage == nil || len(keys) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	for _, key := range keys {
		if err := s.storage.Delete(cleanupCtx, key); err != nil {
			slog.WarnContext(cleanupCtx, "document mail: failed to cleanup temp s3 attachment", "key", key, "error", err)
		}
	}
}

func (s *DocumentMailService) monitorForgeJob(
	parentCtx context.Context,
	deliveryID string,
	baseURL string,
	apiKey string,
	jobID string,
	tempKeys []string,
) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(parentCtx, "document mail: monitorForgeJob panicked", "delivery_id", deliveryID, "panic", r)
		}
	}()
	defer s.cleanupTempS3Keys(parentCtx, tempKeys)

	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Minute)
	defer cancel()

	resp, err := s.forge.OpenProgressStream(ctx, baseURL, apiKey, jobID)
	if err != nil {
		slog.WarnContext(ctx, "document mail: open smtp forge progress stream failed", "delivery_id", deliveryID, "job_id", jobID, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.WarnContext(ctx, "document mail: smtp forge progress stream returned non-2xx", "delivery_id", deliveryID, "status", resp.StatusCode)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 16*1024), 256*1024)
	currentEvent := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if currentEvent == "completed" {
				if _, markErr := platformmiddleware.WithScopedTenantConn(ctx, func(scoped context.Context) (struct{}, error) {
					_, err := s.deliveries.MarkSent(scoped, deliveryID, s.now())
					return struct{}{}, err
				}); markErr != nil {
					slog.WarnContext(ctx, "document mail: mark sent from forge sse failed", "delivery_id", deliveryID, "error", markErr)
				}
				return
			}
			if currentEvent == "failed" {
				if _, markErr := platformmiddleware.WithScopedTenantConn(ctx, func(scoped context.Context) (struct{}, error) {
					_, err := s.deliveries.MarkFailed(scoped, deliveryID, "Pengiriman melalui SMTP Forge gagal")
					return struct{}{}, err
				}); markErr != nil {
					slog.WarnContext(ctx, "document mail: mark failed from forge sse failed", "delivery_id", deliveryID, "error", markErr)
				}
				return
			}
		}
	}
}

func (s *DocumentMailService) recordDeliveryFailure(ctx context.Context, delivery model.EmailDelivery, kind string, sendErr error) (model.EmailDelivery, error) {
	category, code, text := "unknown", 0, "Pengiriman email gagal"
	if classified, ok := mail.AsSendError(sendErr); ok {
		category, code, text = string(classified.Category), classified.Code, classified.Message()
	}
	cause := ""
	if unwrapped := errors.Unwrap(sendErr); unwrapped != nil {
		cause = unwrapped.Error()
	}
	slog.WarnContext(ctx, "document mail send failed",
		"delivery_id", delivery.ID,
		"kind", kind,
		"category", category,
		"smtp_code", code,
		"cause", cause,
	)
	failed, markErr := s.deliveries.MarkFailed(ctx, delivery.ID, text)
	if markErr != nil {
		slog.ErrorContext(ctx, "document mail: recording failed delivery failed", "delivery_id", delivery.ID, "error", markErr)
		delivery.Status = model.EmailDeliveryStatusFailed
		delivery.Error = &text
		return delivery, sendErr
	}
	return failed, sendErr
}

// StreamDeliveryProgress proxies SMTP Forge's SSE progress stream for a
// delivery ID without exposing the API key to the browser.
func (s *DocumentMailService) StreamDeliveryProgress(ctx context.Context, deliveryID string, w http.ResponseWriter, flusher http.Flusher) error {
	var baseURL, apiKey, jobID string
	if raw, ok := s.forgeSessions.Load(deliveryID); ok {
		sess := raw.(forgeTestSession)
		baseURL, apiKey, jobID = sess.baseURL, sess.apiKey, sess.jobID
	} else {
		delivery, err := s.deliveries.GetByID(ctx, deliveryID)
		if errors.Is(err, notificationsrepo.ErrEmailDeliveryNotFound) {
			return ErrDocumentMailDeliveryNotFound
		}
		if err != nil {
			return err
		}
		if delivery.ExternalJobID == nil || strings.TrimSpace(*delivery.ExternalJobID) == "" {
			return ErrDocumentMailDeliveryNotFound
		}
		setting, err := s.repo.GetDocumentMailRecord(ctx)
		if err != nil {
			return err
		}
		creds, err := s.forgeCredentials(setting, nil)
		if err != nil {
			return err
		}
		baseURL, apiKey, jobID = creds.BaseURL, creds.APIKey, *delivery.ExternalJobID
	}

	if s.forge == nil {
		s.forge = mail.NewSMTPForgeSender(nil)
	}
	resp, err := s.forge.OpenProgressStream(ctx, baseURL, apiKey, jobID)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("smtp forge progress returned HTTP %d", resp.StatusCode)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return nil
			}
			flusher.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// SendTest emails the caller's own login address through the configured
// account and records a kind='test' delivery.
func (s *DocumentMailService) SendTest(ctx context.Context, actorID string) (dto.DocumentMailTestResponse, error) {
	return s.SendTestWithOverride(ctx, actorID, nil)
}

// SendTestWithOverride emails the caller's own login address using either
// unsaved form values (when override != nil) or the saved tenant setting.
func (s *DocumentMailService) SendTestWithOverride(ctx context.Context, actorID string, override *dto.TestDocumentMailRequest) (dto.DocumentMailTestResponse, error) {
	user, err := s.repo.GetUserByID(ctx, actorID)
	if err != nil {
		return dto.DocumentMailTestResponse{}, err
	}
	setting, err := s.repo.GetDocumentMailRecord(ctx)
	if err != nil {
		return dto.DocumentMailTestResponse{}, err
	}

	var overridePassword *string
	var overrideAPIKey *string
	if override != nil {
		setting.Enabled = true
		if strings.TrimSpace(override.Provider) != "" {
			setting.Provider = strings.ToLower(strings.TrimSpace(override.Provider))
		}
		if strings.TrimSpace(override.SenderName) != "" || override.Provider != "" {
			setting.SenderName = sanitizeSenderName(override.SenderName)
		}
		if setting.Provider == authrepo.DocumentMailProviderSMTPForge {
			setting.SMTPForgeBaseURL = strings.TrimRight(strings.TrimSpace(override.SMTPForgeBaseURL), "/")
			setting.SMTPForgeAccountID = strings.TrimSpace(override.SMTPForgeAccountID)
			setting.SMTPForgeFromEmail = strings.ToLower(strings.TrimSpace(override.SMTPForgeFromEmail))
			overrideAPIKey = override.SMTPForgeAPIKey
		} else {
			if strings.TrimSpace(override.SMTPUsername) != "" {
				setting.SMTPUsername = strings.ToLower(strings.TrimSpace(override.SMTPUsername))
			}
			if override.SMTPPort == 587 || override.SMTPPort == 465 {
				setting.SMTPPort = override.SMTPPort
			}
			overridePassword = override.SMTPPassword
		}
		setting = authrepo.NormalizeDocumentMailSettingRecord(setting)
	}

	hasSecret := func() bool {
		if setting.Provider == authrepo.DocumentMailProviderSMTPForge {
			return (overrideAPIKey != nil && strings.TrimSpace(*overrideAPIKey) != "") || setting.HasSMTPForgeAPIKey()
		}
		return (overridePassword != nil && strings.TrimSpace(*overridePassword) != "") || setting.HasPassword() || s.effectiveDevSMTPAddr(setting) != ""
	}()

	if override == nil && !s.isReady(setting) {
		return dto.DocumentMailTestResponse{}, ErrDocumentMailNotReady
	}
	if override != nil {
		if !hasSecret {
			return dto.DocumentMailTestResponse{}, ErrDocumentMailNotReady
		}
		if setting.Provider == authrepo.DocumentMailProviderSMTPForge {
			if _, err := mail.NormalizeSMTPForgeAPIBase(setting.SMTPForgeBaseURL); err != nil {
				return dto.DocumentMailTestResponse{}, ErrDocumentMailNotReady
			}
		} else if setting.SMTPUsername == "" || mail.ValidateAddress(setting.SMTPUsername) != nil {
			return dto.DocumentMailTestResponse{}, ErrDocumentMailNotReady
		}
	}

	providerLabel := "Gmail"
	accountDesc := setting.SMTPUsername
	if setting.Provider == authrepo.DocumentMailProviderSMTPForge {
		providerLabel = "SMTP Forge"
		accountDesc = setting.SMTPForgeFromEmail
		if accountDesc == "" {
			accountDesc = setting.SMTPForgeBaseURL
		}
	}

	text, html, err := mail.RenderDocumentEmail(mail.DocumentEmail{
		RecipientName: user.FullName,
		ParagraphsID: []string{
			fmt.Sprintf("Ini adalah email uji dari pengaturan Email Dokumen (%s) di KANTOR.", providerLabel),
			fmt.Sprintf("Jika email ini sampai, slip gaji dan kontrak kerja dapat dikirim melalui %s (%s). Email ini tidak perlu dibalas.", providerLabel, accountDesc),
		},
		ParagraphsEN: []string{
			fmt.Sprintf("This is a test email from the Document Email (%s) settings in KANTOR.", providerLabel),
			fmt.Sprintf("If it reached you, payslips and employment contracts can be sent through %s (%s). No reply is needed.", providerLabel, accountDesc),
		},
		Signature: setting.SenderName,
	})
	if err != nil {
		return dto.DocumentMailTestResponse{}, err
	}

	delivery, err := s.deliverWithSetting(ctx, setting, overridePassword, overrideAPIKey, DocumentMailRequest{
		Kind:            model.EmailDeliveryKindTest,
		Recipient:       user.Email,
		RecipientSource: model.EmailRecipientSourceSelf,
		Subject:         "Email uji Email Dokumen / Document email test",
		Text:            text,
		HTML:            html,
		RequestedBy:     actorID,
	})

	result := dto.DocumentMailTestResponse{
		Sent:          err == nil,
		DeliveryID:    delivery.ID,
		ExternalJobID: delivery.ExternalJobID,
		Recipient:     strings.ToLower(strings.TrimSpace(user.Email)),
	}
	if err != nil {
		sendErr, ok := mail.AsSendError(err)
		if !ok || delivery.ID == "" {
			return dto.DocumentMailTestResponse{}, err
		}
		category := string(sendErr.Category)
		message := sendErr.Message()
		result.ErrorCategory = &category
		result.ErrorMessage = &message
	}
	return result, nil
}

func sanitizeSenderName(value string) string {
	cleaned := strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, value)
	return strings.TrimSpace(cleaned)
}
