// Package ipc — pairing proxy.
//
// POST /v1/pair lets the Tauri shell exchange a display code without ever
// touching the cloud session bearer. The daemon owns the cloud HTTP call to
// /v1/desktop/exchange and the keystore write; the renderer only sees the
// resulting org_id / device_id / expires_at triplet.
package ipc

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
)

// PairDeps groups everything handlePair needs from the daemon. Optional —
// when zero-valued the handler returns 503 so the route still exists.
type PairDeps struct {
	PairingClient *auth.PairingClient
	Keystore      auth.Keystore
	Device        auth.Device
	HwFingerprint string
	// AlreadyPaired returns true if the daemon already has a stored
	// session bundle (used to short-circuit double-pairing).
	AlreadyPaired func() bool
}

// pairRequest is the renderer-facing body.
type pairRequest struct {
	DisplayCode string `json:"display_code"`
}

// pairResponse is the renderer-facing success body.
type pairResponse struct {
	OrgID     string `json:"org_id"`
	DeviceID  string `json:"device_id"`
	ExpiresAt string `json:"expires_at"`
}

// handlePair is wired in routes(). Returns 503 if PairDeps not configured.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if s.deps.Pair == nil || s.deps.Pair.PairingClient == nil || s.deps.Pair.Keystore == nil {
		writeErr(w, http.StatusServiceUnavailable, "pairing not configured")
		return
	}
	var body pairRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Internal detail (decode error) stays in the daemon log; the
		// renderer only sees a stable code so we don't leak handler text.
		slog.Warn("pair: decode body", "err", err)
		writeErr(w, http.StatusBadRequest, "invalid_json")
		return
	}
	code := strings.TrimSpace(body.DisplayCode)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing display_code")
		return
	}
	if s.deps.Pair.AlreadyPaired != nil && s.deps.Pair.AlreadyPaired() {
		writeErr(w, http.StatusConflict, "already_paired")
		return
	}

	resp, err := s.deps.Pair.PairingClient.Exchange(r.Context(), auth.ExchangeRequest{
		DisplayCode:   code,
		Device:        s.deps.Pair.Device,
		HwFingerprint: s.deps.Pair.HwFingerprint,
	})
	if err != nil {
		// Cloud signals an invalid pairing code via auth.ErrUnauthorized;
		// anything else is bad-gateway so the renderer can distinguish
		// "wrong code" from "transport down".
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, http.StatusUnauthorized, "invalid_code")
			return
		}
		s.logger().Warn("pair: exchange failed", "err", err)
		writeErr(w, http.StatusBadGateway, "cloud_unreachable")
		return
	}

	bundle := auth.SessionBundle{
		DeviceID:         resp.Device.ID,
		OrganizationID:   resp.Device.OrganizationID,
		SessionJWT:       resp.Session.Token,
		SessionExpiresAt: resp.Session.ExpiresAt,
		RefreshToken:     resp.Refresh.Token,
		RefreshExpiresAt: resp.Refresh.ExpiresAt,
		HwFingerprint:    s.deps.Pair.HwFingerprint,
	}
	if err := s.deps.Pair.Keystore.Save(bundle); err != nil {
		s.logger().Error("pair: persist session", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, pairResponse{
		OrgID:     resp.Device.OrganizationID,
		DeviceID:  resp.Device.ID,
		ExpiresAt: resp.Session.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

