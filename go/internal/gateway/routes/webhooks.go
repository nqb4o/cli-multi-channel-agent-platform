package routes

import (
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/openclaw/agent-platform/internal/gateway"
)

func mountWebhooks(r chi.Router, app *gateway.App) {
	r.Post("/channels/{type}/webhook", makeWebhookHandler(app))
}

func makeWebhookHandler(app *gateway.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channelType := chi.URLParam(r, "type")
		if channelType == "" {
			writeError(w, http.StatusNotFound, "unknown channel type: ")
			return
		}
		adapter := app.Channels.Get(channelType)
		if adapter == nil {
			writeError(w, http.StatusNotFound, "unknown channel type: "+channelType)
			return
		}

		// Cap body at 1 MiB to mirror Python's effective Starlette default.
		r.Body = http.MaxBytesReader(w, r.Body, defaultMaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
			return
		}

		// Step 2 — signature verification.
		if !safeVerify(adapter, r.Header, body) {
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}

		// Step 3 — parse.
		msg, perr := adapter.ParseIncoming(body)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "malformed webhook body: "+perr.Error())
			return
		}
		if msg == nil {
			writeError(w, http.StatusBadRequest, "adapter returned nil message")
			return
		}

		// Step 4 — idempotency.
		ctx, cancel := requestCtxWithTimeout(r, 2*time.Second)
		defer cancel()
		isFirst, err := app.Idempotency.Claim(ctx, channelType, msg.MessageID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "idempotency cache error: "+err.Error())
			return
		}
		if !isFirst {
			writeJSON(w, http.StatusOK, map[string]string{
				"status":     "duplicate",
				"message_id": msg.MessageID,
			})
			return
		}

		// Step 5 — DB lookup.
		routing, err := app.ChannelsRepo.LookupRouting(ctx, channelType, msg.ChannelID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "channel lookup failed: "+err.Error())
			return
		}
		if routing == nil {
			writeError(w, http.StatusNotFound, "channel not registered")
			return
		}

		// Step 6 — enqueue.
		runID := uuid.NewString()

		// Build payload: copy adapter payload then overlay the normalized
		// fields the adapter parsed, so consumers don't need to re-parse the
		// channel-native body. setdefault semantics: don't clobber adapter keys.
		payload := map[string]any{}
		for k, v := range msg.Payload {
			payload[k] = v
		}
		if _, ok := payload["text"]; !ok {
			payload["text"] = msg.Text
		}
		if _, ok := payload["sender_id"]; !ok {
			payload["sender_id"] = msg.SenderID
		}
		if _, ok := payload["attachments"]; !ok {
			atts := make([]any, 0, len(msg.Attachments))
			for _, a := range msg.Attachments {
				atts = append(atts, a)
			}
			payload["attachments"] = atts
		}
		encoded, err := gateway.EncodeMessagePayload(payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encode payload: "+err.Error())
			return
		}

		receivedAt := msg.ReceivedAt
		if receivedAt == "" {
			receivedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}

		job := gateway.AgentRunJob{
			RunID:      runID,
			UserID:     routing.UserID,
			AgentID:    routing.AgentID,
			ChannelID:  routing.ChannelID,
			ThreadID:   msg.ThreadID,
			Message:    encoded,
			ReceivedAt: receivedAt,
		}
		if _, err := app.Queue.Enqueue(ctx, job); err != nil {
			writeError(w, http.StatusInternalServerError, "enqueue failed: "+err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status": "accepted",
			"run_id": runID,
		})
	}
}

// safeVerify swallows panics from buggy adapters, returning false on any
// failure. Mirrors the Python "broad-except → 401" branch.
func safeVerify(adapter interface {
	VerifySignature(http.Header, []byte) bool
}, h http.Header, body []byte) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			ok = false
		}
	}()
	return adapter.VerifySignature(h, body)
}
