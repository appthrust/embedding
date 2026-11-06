package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appthrust/appserver-go/apphealth"
	"github.com/appthrust/appserver-go/appserver/health"
	"github.com/appthrust/appserver-go/appserver/ollamasupervisor"
	"github.com/stretchr/testify/require"
)

func handlerForTest(t *testing.T, upstream string, ready func() bool) http.Handler {
	t.Helper()
	u, err := url.Parse(upstream)
	require.NoError(t, err)
	return newHandler("embeddinggemma", u, ready, &health.Outcomes{})
}

func requestForTest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func embeddingResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "model": "embeddinggemma",
		"data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": make([]float32, embeddingDimensions)}},
	})
}

func TestProxyForcesModelAndOnlyForwardsEmbeddings(t *testing.T) {
	requests := make(chan *http.Request, 8)
	bodies := make(chan map[string]any, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode forwarded body: %v", err)
		}
		requests <- r
		bodies <- body
		embeddingResponse(w)
	}))
	defer upstream.Close()
	h := handlerForTest(t, upstream.URL, func() bool { return true })
	for _, model := range []string{"", `,"model":""`, `,"model":"embeddinggemma"`} {
		r := httptest.NewRequest(http.MethodPost, "/v1/embeddings?ignored=1", strings.NewReader(`{"input":["hello","world"],"encoding_format":"float"`+model+`}`))
		r.Header.Set("Authorization", "Bearer private-user-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "application/json", w.Header().Get("Content-Type"))
		forwarded, body := <-requests, <-bodies
		require.Equal(t, http.MethodPost, forwarded.Method)
		require.Equal(t, "/v1/embeddings", forwarded.URL.Path)
		require.Empty(t, forwarded.URL.RawQuery)
		require.Empty(t, forwarded.Header.Get("Authorization"))
		require.Equal(t, "embeddinggemma", body["model"])
		require.Equal(t, []any{"hello", "world"}, body["input"])
		require.Equal(t, "float", body["encoding_format"])
	}
	for _, path := range []string{"/api/tags", "/api/pull", "/api/delete", "/v1/models", "/v1/embeddings/extra", "/other"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			require.Equal(t, http.StatusNotFound, requestForTest(h, method, path, `{}`).Code, path)
		}
	}
	require.Equal(t, http.StatusMethodNotAllowed, requestForTest(h, http.MethodGet, "/v1/embeddings", "").Code)
	require.Empty(t, requests)
	root := requestForTest(h, http.MethodGet, "/", "")
	require.JSONEq(t, `{"model":"embeddinggemma","dimensions":768}`, root.Body.String())
}

func TestProxyRejectsInvalidAndOversizedRequests(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		embeddingResponse(w)
	}))
	defer upstream.Close()
	h := handlerForTest(t, upstream.URL, func() bool { return true })
	for _, body := range []string{``, `null`, `[]`, `{`, `{} {}`, `{"input":"hi","model":"other"}`, `{"input":"hi","model":42}`} {
		w := requestForTest(h, http.MethodPost, "/v1/embeddings", body)
		require.Equal(t, http.StatusBadRequest, w.Code, body)
		require.Contains(t, w.Body.String(), "invalid_request")
	}
	prefix, suffix := `{"input":"`, `"}`
	body := prefix + strings.Repeat("x", maxRequestBytes-len(prefix)-len(suffix)) + suffix
	require.Equal(t, http.StatusOK, requestForTest(h, http.MethodPost, "/v1/embeddings", body).Code)
	w := requestForTest(h, http.MethodPost, "/v1/embeddings", body+" ")
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Contains(t, w.Body.String(), "request_too_large")
	require.Equal(t, int32(1), calls.Load())
}

func TestProxyRejectsFifthInFlightRequest(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		embeddingResponse(w)
	}))
	defer upstream.Close()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h := handlerForTest(t, upstream.URL, func() bool { return true })
	results := make(chan int, 4)
	for range 4 {
		go func() { results <- requestForTest(h, http.MethodPost, "/v1/embeddings", `{"input":"hello"}`).Code }()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("four requests did not reach upstream")
		}
	}
	w := requestForTest(h, http.MethodPost, "/v1/embeddings", `{"input":"fifth"}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "embedding_busy")
	unblock()
	for range 4 {
		select {
		case status := <-results:
			require.Equal(t, http.StatusOK, status)
		case <-time.After(5 * time.Second):
			t.Fatal("admitted request did not complete")
		}
	}
	require.Equal(t, http.StatusOK, requestForTest(h, http.MethodPost, "/v1/embeddings", `{"input":"after release"}`).Code)
}

func TestProxyDoesNotFollowRedirectsAndPreservesUpstreamErrors(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected management request: %s", r.URL.Path)
		}
		if calls.Load() == 1 {
			http.Redirect(w, r, "/api/pull", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"input is required"}}`)
	}))
	defer upstream.Close()
	h := handlerForTest(t, upstream.URL, func() bool { return true })
	w := requestForTest(h, http.MethodPost, "/v1/embeddings", `{}`)
	require.Equal(t, http.StatusTemporaryRedirect, w.Code)
	require.Equal(t, int32(1), calls.Load())
	w = requestForTest(h, http.MethodPost, "/v1/embeddings", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.JSONEq(t, `{"error":{"message":"input is required"}}`, w.Body.String())
}

func TestProxyDeadlineAndUnavailable(t *testing.T) {
	h := handlerForTest(t, "http://127.0.0.1:1", func() bool { return true })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)
	require.Equal(t, http.StatusGatewayTimeout, w.Code)
	require.Contains(t, w.Body.String(), "embedding_timeout")
	require.Equal(t, 120*time.Second, upstreamClient().Timeout)
	w = requestForTest(h, http.MethodPost, "/v1/embeddings", `{"input":"hello"}`)
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "embedding_upstream_unavailable")
}

func TestReadinessRequiresWarmupAndRecoversAfterOutage(t *testing.T) {
	var available atomic.Bool
	warming, allowWarm := make(chan struct{}, 8), make(chan struct{}, 8)
	var warms atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if !available.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/v1/embeddings":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "embeddinggemma" || body["input"] == "" {
				t.Errorf("invalid warm-up: %v %v", body, err)
			}
			warms.Add(1)
			warming <- struct{}{}
			select {
			case <-allowWarm:
				embeddingResponse(w)
			case <-r.Context().Done():
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	s := ollamasupervisor.New(ollamasupervisor.Config{
		Upstream: upstream.URL, Client: upstreamClient(), Warmup: warmup("embeddinggemma"),
		PollInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	h := handlerForTest(t, upstream.URL, s.Ready)
	require.Equal(t, http.StatusOK, requestForTest(h, http.MethodGet, "/healthz", "").Code)
	require.Equal(t, http.StatusServiceUnavailable, requestForTest(h, http.MethodGet, "/readyz", "").Code)
	require.Equal(t, http.StatusServiceUnavailable, requestForTest(h, http.MethodPost, "/v1/embeddings", `{}`).Code)
	available.Store(true)
	select {
	case <-warming:
	case <-time.After(5 * time.Second):
		t.Fatal("warm-up never started")
	}
	require.False(t, s.Ready(), "tags alone must not make the model ready")
	allowWarm <- struct{}{}
	require.Eventually(t, s.Ready, 5*time.Second, time.Millisecond)
	require.Equal(t, http.StatusOK, requestForTest(h, http.MethodGet, "/readyz", "").Code)
	available.Store(false)
	require.Eventually(t, func() bool { return !s.Ready() }, 5*time.Second, time.Millisecond)
	require.Equal(t, int32(1), warms.Load())
	available.Store(true)
	select {
	case <-warming:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery warm-up never started")
	}
	require.False(t, s.Ready())
	allowWarm <- struct{}{}
	require.Eventually(t, s.Ready, 5*time.Second, time.Millisecond)
	cancel()
	<-done
	require.False(t, s.Ready())
}

func TestWarmupRejectsInvalidEmbedding(t *testing.T) {
	for _, response := range []string{`{}`, `{"data":[{"embedding":[1,2]}]}`, `not json`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, response)
		}))
		require.Error(t, warmup("embeddinggemma")(context.Background(), upstreamClient(), upstream.URL))
		s := ollamasupervisor.New(ollamasupervisor.Config{
			Upstream: upstream.URL, Client: upstreamClient(), Warmup: warmup("embeddinggemma"),
		})
		require.False(t, s.Ready())
		upstream.Close()
	}
}

func TestReleaseIdentityEnvironmentAndFile(t *testing.T) {
	values := map[string]string{
		"EMBEDDING_RELEASE_VERSION": "release", "EMBEDDING_SOURCE_REVISION": "",
		"EMBEDDING_ARTIFACT_DIGEST": "digest",
		"EMBEDDING_RELEASE_FILE":    "",
	}
	for key := range values {
		t.Setenv(key, "")
	}
	handler := func() http.Handler { return handlerForTest(t, "http://127.0.0.1:1", func() bool { return false }) }
	require.Equal(t, http.StatusServiceUnavailable, requestForTest(handler(), http.MethodGet, "/releasez", "").Code)
	file := filepath.Join(t.TempDir(), "release.json")
	stamp := `{"releaseVersion":"file","sourceRevision":"file-revision","artifactDigest":"file-digest"}`
	require.NoError(t, os.WriteFile(file, []byte(stamp), 0600))
	t.Setenv("EMBEDDING_RELEASE_FILE", file)
	t.Setenv("EMBEDDING_RELEASE_VERSION", "partial-env")
	w := requestForTest(handler(), http.MethodGet, "/releasez", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.JSONEq(t, stamp, w.Body.String())
	for key, value := range values {
		if key != "EMBEDDING_RELEASE_FILE" {
			t.Setenv(key, value)
		}
	}
	w = requestForTest(handler(), http.MethodGet, "/releasez", "")
	require.JSONEq(t, `{"releaseVersion":"release","artifactDigest":"digest"}`, w.Body.String())
	t.Setenv("EMBEDDING_ARTIFACT_DIGEST", "")
	t.Setenv("EMBEDDING_RELEASE_FILE", file+".missing")
	require.Equal(t, http.StatusServiceUnavailable, requestForTest(handler(), http.MethodGet, "/releasez", "").Code)
}

func TestEnvironmentDefaultsAndOverrides(t *testing.T) {
	for _, name := range []string{"EMBEDDING_MODEL", "EMBEDDING_UPSTREAM", "EMBEDDING_OLLAMA_BIN", "PLATFORM_PROJECT_ID"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	cfg, err := configFromEnv()
	require.NoError(t, err)
	require.Equal(t, "embeddinggemma", cfg.model)
	require.Equal(t, "http://127.0.0.1:11434", cfg.upstream.String())
	require.Equal(t, "ollama", cfg.ollamaBin)
	require.Empty(t, cfg.projectID)
	t.Setenv("EMBEDDING_OLLAMA_BIN", "")
	t.Setenv("EMBEDDING_MODEL", "embeddinggemma:latest")
	t.Setenv("EMBEDDING_UPSTREAM", "http://127.0.0.1:11435/")
	t.Setenv("PLATFORM_PROJECT_ID", "pj-test")
	cfg, err = configFromEnv()
	require.NoError(t, err)
	require.Empty(t, cfg.ollamaBin)
	require.Equal(t, "embeddinggemma:latest", cfg.model)
	require.Equal(t, "http://127.0.0.1:11435", cfg.upstream.String())
	require.Equal(t, "pj-test", cfg.projectID)
	for _, raw := range []string{"file:///tmp/socket", "http://user:password@localhost", "http://localhost/api", "http://localhost?x=1", "http://localhost#fragment"} {
		t.Setenv("EMBEDDING_UPSTREAM", raw)
		_, err := configFromEnv()
		require.ErrorContains(t, err, "EMBEDDING_UPSTREAM must be an HTTP(S) origin")
	}
}

func TestApplicationHealthTracksEmbeddingsWithoutChangingReadiness(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	h := handlerForTest(t, upstream.URL, func() bool { return true })
	for range 3 {
		require.Equal(t, http.StatusBadGateway, requestForTest(h, http.MethodPost, "/v1/embeddings", `{"input":"hello"}`).Code)
	}
	w := requestForTest(h, http.MethodGet, health.Path, "")
	require.Equal(t, http.StatusOK, w.Code)
	var report apphealth.Report
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &report))
	require.NoError(t, report.Validate([]string{"embeddings"}))
	require.Equal(t, apphealth.StateFailing, report.Capabilities[0].State)
	require.Equal(t, apphealth.ReasonUpstreamUnavailable, report.Capabilities[0].Reason)
	require.Equal(t, http.StatusOK, requestForTest(h, http.MethodGet, "/readyz", "").Code)
	r := httptest.NewRequest(http.MethodGet, health.Path, nil)
	r.Header.Set(apphealth.PublicRouteHeader, "true")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
}
