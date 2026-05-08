package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
)

// ─── helpers ────────────────────────────────────────────────────────────

// fakeKeystore is an in-memory Keystore implementation that records Save /
// Load / Clear calls. The cloud unpair stub takes the place of the real
// PairingClient; here we only need Keystore to satisfy the IPC handler's
// Pair.Keystore field — and we want to assert Clear() actually fired.
type fakeKeystore struct {
	mu       sync.Mutex
	bundle   auth.SessionBundle
	hasBundle bool
	cleared  bool
}

func (k *fakeKeystore) Save(b auth.SessionBundle) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bundle = b
	k.hasBundle = true
	return nil
}
func (k *fakeKeystore) Load() (auth.SessionBundle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.hasBundle {
		return auth.SessionBundle{}, auth.ErrNoSession
	}
	return k.bundle, nil
}
func (k *fakeKeystore) Clear() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cleared = true
	k.hasBundle = false
	k.bundle = auth.SessionBundle{}
	return nil
}
func (k *fakeKeystore) Backend() string { return "fake" }

// newUnpairServer wires a Server with PairDeps + a stub cloud round-tripper
// configured to return the supplied status code. Used by every unpair test
// to model "cloud says X" without spinning a real cloud.
func newUnpairServer(t *testing.T, cloudStatus int, cloudBody string, cloudErr error) (*Server, *fakeKeystore, *jobqueue.Store, *httptest.Server) {
	t.Helper()
	store, err := jobqueue.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	resMgr := resources.New(resources.Limits{}, t.TempDir())
	ks := &fakeKeystore{}
	// Seed a session so the unpair handler proceeds past the "not paired"
	// short-circuit.
	if err := ks.Save(auth.SessionBundle{
		DeviceID:       "device-1",
		OrganizationID: "org-1",
		SessionJWT:     "stub.jwt.token",
		HwFingerprint:  "fp-stub",
	}); err != nil {
		t.Fatalf("seed bundle: %v", err)
	}

	// Stub cloud server — answers DELETE /v1/desktop/devices/self with the
	// configured status / body / err.
	var cloudSrv *httptest.Server
	if cloudErr != nil {
		// Simulate transport error by using a server that closes the
		// connection.
		cloudSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
	} else {
		cloudSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/desktop/devices/self" || r.Method != http.MethodDelete {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer stub.jwt.token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(cloudStatus)
			_, _ = w.Write([]byte(cloudBody))
		}))
	}
	t.Cleanup(cloudSrv.Close)

	pairingClient := auth.NewPairingClient(cloudSrv.URL)

	paused := false
	srv := &Server{
		token: testToken,
		deps: Deps{
			Store:      store,
			Resources:  resMgr,
			Pause:      func() { paused = true },
			Resume:     func() { paused = false },
			Paused:     func() bool { return paused },
			AppVersion: "test",
			Pair: &PairDeps{
				PairingClient: pairingClient,
				Keystore:      ks,
				HwFingerprint: "fp-stub",
				AlreadyPaired: func() bool { _, err := ks.Load(); return err == nil },
			},
		},
	}
	mux := http.NewServeMux()
	srv.routes(mux)
	httpSrv := httptest.NewServer(sseQueryAuthShim(srv.middleware(mux)))
	t.Cleanup(httpSrv.Close)

	return srv, ks, store, httpSrv
}

// ─── tests ────────────────────────────────────────────────────────────

func TestUnpair_HappyPath_ClearsKeystoreAndSweepsJobs(t *testing.T) {
	_, ks, store, httpSrv := newUnpairServer(t, http.StatusNoContent, "", nil)

	// Seed an active job that should be swept to failed.
	ctx := context.Background()
	if err := store.CreateJob(ctx, jobqueue.InsertJobInput{
		ID: "j1", WorkloadID: "w1", OrganizationID: "org-1",
		WorkloadType: "video:extract-audio", WorkloadVersion: 1, PayloadJSON: "{}",
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", "{}", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, body)
	}

	var got unpairResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClearedJobs != 1 {
		t.Fatalf("ClearedJobs = %d; want 1", got.ClearedJobs)
	}
	if got.Forced {
		t.Fatalf("Forced = true; want false on happy path")
	}

	if !ks.cleared {
		t.Fatalf("keystore.Clear was not called")
	}

	// Job should be failed with reason 'unpaired'.
	j, err := store.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.Status != jobqueue.StatusFailed {
		t.Fatalf("job status = %q; want failed", j.Status)
	}
}

func TestUnpair_NotPaired_Returns409(t *testing.T) {
	_, ks, _, httpSrv := newUnpairServer(t, http.StatusNoContent, "", nil)
	// Pre-clear the keystore so the handler hits the "not paired" branch.
	if err := ks.Clear(); err != nil {
		t.Fatalf("clear seed: %v", err)
	}
	ks.cleared = false // Reset so we can assert it was NOT re-cleared.

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if ks.cleared {
		t.Fatalf("keystore.Clear should NOT fire on already-not-paired")
	}
}

func TestUnpair_CloudUnreachable_NoForce_Returns502(t *testing.T) {
	_, ks, _, httpSrv := newUnpairServer(t, 0, "", errors.New("cloud down"))

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", `{"force":false}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	if ks.cleared {
		t.Fatalf("keystore.Clear MUST NOT fire on cloud-unreachable + !force")
	}
}

func TestUnpair_CloudUnreachable_WithForce_ClearsLocal(t *testing.T) {
	_, ks, _, httpSrv := newUnpairServer(t, 0, "", errors.New("cloud down"))

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", `{"force":true}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 on force, got %d (%s)", resp.StatusCode, body)
	}
	var got unpairResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Forced {
		t.Fatalf("Forced = false; want true when cloud fails + force=true")
	}
	if !ks.cleared {
		t.Fatalf("keystore.Clear should fire under force=true")
	}
}

func TestUnpair_Cloud401_TreatedAsSuccess(t *testing.T) {
	_, ks, _, httpSrv := newUnpairServer(t, http.StatusUnauthorized, "expired", nil)

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 on cloud 401, got %d (%s)", resp.StatusCode, body)
	}
	if !ks.cleared {
		t.Fatalf("keystore.Clear should still fire when cloud returns 401")
	}
}

func TestUnpair_Cloud4xxOther_Returns502(t *testing.T) {
	_, ks, _, httpSrv := newUnpairServer(t, http.StatusForbidden, "fingerprint mismatch", nil)

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/v1/unpair", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on cloud 403, got %d", resp.StatusCode)
	}
	if ks.cleared {
		t.Fatalf("keystore.Clear MUST NOT fire on non-401 4xx (user should retry)")
	}
}

func TestUnpair_RequiresPOST(t *testing.T) {
	_, _, _, httpSrv := newUnpairServer(t, http.StatusNoContent, "", nil)

	resp := authedRequest(t, http.MethodGet, httpSrv.URL+"/v1/unpair", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/unpair: want 405, got %d", resp.StatusCode)
	}
}

func TestUnpair_RequiresAuth(t *testing.T) {
	_, _, _, httpSrv := newUnpairServer(t, http.StatusNoContent, "", nil)

	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/unpair", strings.NewReader(""))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", resp.StatusCode)
	}
}
