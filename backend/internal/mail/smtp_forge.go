package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultForgeHTTPTimeout = 20 * time.Second

// SMTPForgeCredentials are the per-tenant SMTP Forge gateway settings.
type SMTPForgeCredentials struct {
	BaseURL    string
	APIKey     string
	AccountID  string
	FromEmail  string
	SenderName string
}

// SMTPForgeAttachment is an attachment passed to SMTP Forge by presigned URL.
type SMTPForgeAttachment struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

// SMTPForgeSender delivers document emails through an SMTP Forge HTTP gateway.
type SMTPForgeSender struct {
	client *http.Client
}

// NewSMTPForgeSender creates a new SMTP Forge HTTP client.
func NewSMTPForgeSender(client *http.Client) *SMTPForgeSender {
	if client == nil {
		client = &http.Client{Timeout: defaultForgeHTTPTimeout}
	}
	return &SMTPForgeSender{client: client}
}

// NormalizeSMTPForgeAPIBase ensures the base URL points to the /api/v1 prefix.
func NormalizeSMTPForgeAPIBase(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", configError("URL SMTP Forge belum diisi")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", configError("URL SMTP Forge tidak valid")
	}
	if !strings.HasSuffix(parsed.Path, "/api/v1") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1"
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

type smtpForgeSendRequest struct {
	FromEmail   string                `json:"from_email,omitempty"`
	FromName    string                `json:"from_name,omitempty"`
	To          string                `json:"to"`
	Cc          []string              `json:"cc,omitempty"`
	Subject     string                `json:"subject"`
	HTML        string                `json:"html"`
	Attachments []SMTPForgeAttachment `json:"attachments,omitempty"`
	SMTPAccount string                `json:"smtp_account,omitempty"`
}

type smtpForgeSendResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Message string `json:"message"`
		JobID   string `json:"job_id"`
		Status  string `json:"status"`
	} `json:"data"`
	Message string `json:"message"`
}

// Send queues an email via POST /api/v1/emails and returns the gateway job_id.
func (s *SMTPForgeSender) Send(
	ctx context.Context,
	creds SMTPForgeCredentials,
	msg Message,
	attachments []SMTPForgeAttachment,
) (string, error) {
	apiBase, err := NormalizeSMTPForgeAPIBase(creds.BaseURL)
	if err != nil {
		return "", err
	}
	apiKey := strings.TrimSpace(creds.APIKey)
	if apiKey == "" {
		return "", configError("API key SMTP Forge belum diisi")
	}
	to := strings.TrimSpace(msg.To)
	if err := validateBareAddress(to); err != nil {
		return "", &SendError{Category: CategoryMessage, cause: err}
	}
	for _, cc := range msg.Cc {
		if err := validateBareAddress(cc); err != nil {
			return "", &SendError{Category: CategoryMessage, cause: err}
		}
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return "", &SendError{Category: CategoryMessage, cause: errors.New("subject is required")}
	}

	htmlBody := strings.TrimSpace(msg.HTML)
	if htmlBody == "" {
		htmlBody = "<pre>" + html.EscapeString(msg.Text) + "</pre>"
	}

	payload := smtpForgeSendRequest{
		FromEmail:   strings.TrimSpace(creds.FromEmail),
		FromName:    strings.TrimSpace(creds.SenderName),
		To:          to,
		Cc:          msg.Cc,
		Subject:     strings.TrimSpace(msg.Subject),
		HTML:        htmlBody,
		Attachments: attachments,
		SMTPAccount: strings.TrimSpace(creds.AccountID),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", &SendError{Category: CategoryMessage, cause: err}
	}

	endpoint := apiBase + "/emails"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &SendError{Category: CategoryConfig, Detail: "URL SMTP Forge tidak valid", cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Mailer-Api-Key", apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		if isTimeout(err) {
			return "", &SendError{Category: CategoryTimeout, Addr: apiBase, cause: err}
		}
		return "", &SendError{Category: CategoryConnect, Addr: apiBase, cause: err}
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed smtpForgeSendResponse
	_ = json.Unmarshal(respBytes, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", classifyForgeHTTPError(resp.StatusCode, apiBase, parsed.Message, string(respBytes))
	}

	jobID := strings.TrimSpace(parsed.Data.JobID)
	if jobID == "" {
		return "", &SendError{Category: CategoryRejected, Code: resp.StatusCode, Addr: apiBase, cause: errors.New("smtp forge response missing job_id")}
	}
	return jobID, nil
}

// OpenProgressStream opens the SSE stream GET /api/v1/emails/{jobID}/progress.
// The caller is responsible for closing resp.Body.
func (s *SMTPForgeSender) OpenProgressStream(ctx context.Context, baseURL, apiKey, jobID string) (*http.Response, error) {
	apiBase, err := NormalizeSMTPForgeAPIBase(baseURL)
	if err != nil {
		return nil, err
	}
	trimmedKey := strings.TrimSpace(apiKey)
	if trimmedKey == "" {
		return nil, configError("API key SMTP Forge belum diisi")
	}
	trimmedJobID := strings.TrimSpace(jobID)
	if trimmedJobID == "" {
		return nil, configError("job_id SMTP Forge tidak valid")
	}

	endpoint := fmt.Sprintf("%s/emails/%s/progress", apiBase, url.PathEscape(trimmedJobID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Mailer-Api-Key", trimmedKey)

	// Use a dedicated client without a short Timeout so SSE can stay open.
	streamClient := &http.Client{Transport: s.client.Transport}
	return streamClient.Do(req)
}

func classifyForgeHTTPError(status int, addr string, msg string, raw string) *SendError {
	causeMsg := strings.TrimSpace(msg)
	if causeMsg == "" {
		causeMsg = strings.TrimSpace(raw)
	}
	cause := fmt.Errorf("smtp forge HTTP %d: %s", status, causeMsg)

	switch {
	case status == http.StatusUnauthorized:
		return &SendError{Category: CategoryAuth, Code: status, Addr: addr, cause: cause}
	case status == http.StatusForbidden:
		return &SendError{Category: CategoryAuth, Code: status, Addr: addr, cause: cause}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return &SendError{Category: CategoryMessage, Code: status, Addr: addr, cause: cause}
	case status == http.StatusTooManyRequests || status >= 500:
		return &SendError{Category: CategoryTemporary, Code: status, Addr: addr, cause: cause}
	default:
		return &SendError{Category: CategoryRejected, Code: status, Addr: addr, cause: cause}
	}
}

