package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSMTPForgeSenderSendSuccess(t *testing.T) {
	var gotAPIKey string
	var gotPayload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/emails" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAPIKey = r.Header.Get("X-Mailer-Api-Key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotPayload)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"success":true,"data":{"message":"Email queued successfully","job_id":"job-123","status":"queued"}}`))
	}))
	defer server.Close()

	sender := NewSMTPForgeSender(server.Client())
	jobID, err := sender.Send(
		context.Background(),
		SMTPForgeCredentials{
			BaseURL:    server.URL,
			APIKey:     "secret-key-123",
			AccountID:  "b2fe839c-0f7f-48fe-872f-9dc37d51af65",
			FromEmail:  "hr@example.com",
			SenderName: "HR Kantor",
		},
		Message{
			To:      "employee@example.com",
			Subject: "Slip Gaji",
			HTML:    "<p>Halo</p>",
		},
		[]SMTPForgeAttachment{{Filename: "slip.pdf", URL: "https://s3.example.com/slip.pdf"}},
	)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if jobID != "job-123" {
		t.Fatalf("expected job-123, got %q", jobID)
	}
	if gotAPIKey != "secret-key-123" {
		t.Fatalf("expected X-Mailer-Api-Key, got %q", gotAPIKey)
	}
	if gotPayload["smtp_account"] != "b2fe839c-0f7f-48fe-872f-9dc37d51af65" {
		t.Fatalf("expected smtp_account in payload, got %#v", gotPayload["smtp_account"])
	}
}

func TestSMTPForgeSenderAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"message":"Invalid API key"}`))
	}))
	defer server.Close()

	sender := NewSMTPForgeSender(server.Client())
	_, err := sender.Send(
		context.Background(),
		SMTPForgeCredentials{BaseURL: server.URL + "/api/v1", APIKey: "bad-key"},
		Message{To: "employee@example.com", Subject: "Test", HTML: "<p>Hi</p>"},
		nil,
	)
	sendErr, ok := AsSendError(err)
	if !ok {
		t.Fatalf("expected *SendError, got %T: %v", err, err)
	}
	if sendErr.Category != CategoryAuth {
		t.Fatalf("expected CategoryAuth, got %s", sendErr.Category)
	}
}

