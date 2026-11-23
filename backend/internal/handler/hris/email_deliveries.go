package hris

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kana-consultant/kantor/backend/internal/response"
	authservice "github.com/kana-consultant/kantor/backend/internal/service/auth"
	hrisservice "github.com/kana-consultant/kantor/backend/internal/service/hris"
)

// EmailDeliveriesHandler serves GET /hris/email-deliveries: the delivery
// history ('Riwayat Pengiriman') of one payslip or contract. The required
// permission follows the document kind (hris:payslip:view or
// hris:contract:view), checked against the stored rows; admin test e-mails
// are never listed.
type EmailDeliveriesHandler struct {
	mailer *hrisservice.DocumentMailer
}

func NewEmailDeliveriesHandler(mailer *hrisservice.DocumentMailer) *EmailDeliveriesHandler {
	return &EmailDeliveriesHandler{mailer: mailer}
}

func (h *EmailDeliveriesHandler) RegisterRoutes(router chi.Router) {
	router.Get("/", h.list)
	router.Get("/{deliveryID}/progress", h.streamProgress)
}

func (h *EmailDeliveriesHandler) streamProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalOrUnauthorized(w, r); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		response.WriteError(w, http.StatusInternalServerError, "STREAM_UNSUPPORTED", "Streaming is not supported", nil)
		return
	}
	deliveryID := strings.TrimSpace(chi.URLParam(r, "deliveryID"))
	if deliveryID == "" {
		response.WriteError(w, http.StatusBadRequest, "VALIDATION_ERROR", "deliveryID is required", nil)
		return
	}
	if err := h.mailer.StreamDeliveryProgress(r.Context(), deliveryID, w, flusher); err != nil {
		if errors.Is(err, authservice.ErrDocumentMailDeliveryNotFound) {
			response.WriteError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		response.WriteInternalError(r.Context(), w, err, "Gagal memuat progres pengiriman email")
	}
}

func (h *EmailDeliveriesHandler) list(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalOrUnauthorized(w, r)
	if !ok {
		return
	}
	referenceType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("reference_type")))
	referenceID := strings.TrimSpace(r.URL.Query().Get("reference_id"))
	if referenceType == "" || referenceID == "" {
		response.WriteError(w, http.StatusBadRequest, "VALIDATION_ERROR", "reference_type dan reference_id wajib diisi", map[string]string{"reference_type": "required", "reference_id": "required"})
		return
	}
	result, err := h.mailer.ListDeliveries(r.Context(), referenceType, referenceID, documentPermissionChecker(principal), documentViewer(r, principal))
	if err != nil {
		writeDocumentError(r.Context(), w, err, "Terjadi kesalahan saat memuat riwayat pengiriman")
		return
	}
	response.WriteJSON(w, http.StatusOK, result, nil)
}
