package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAICodexTicketFailure(t *testing.T) {
	require.Equal(t, openAICodexTicketFailureNetwork, classifyOpenAICodexTicketFailure(0, io.EOF))
	// A transport error wins even when a status somehow came along.
	require.Equal(t, openAICodexTicketFailureNetwork, classifyOpenAICodexTicketFailure(500, io.EOF))
	require.Equal(t, openAICodexTicketFailureBadTicket, classifyOpenAICodexTicketFailure(200, nil))
	require.Equal(t, openAICodexTicketFailureRateLimited, classifyOpenAICodexTicketFailure(429, nil))
	require.Equal(t, openAICodexTicketFailureUnauthed, classifyOpenAICodexTicketFailure(401, nil))
	require.Equal(t, openAICodexTicketFailureForbidden, classifyOpenAICodexTicketFailure(403, nil))
	require.Equal(t, openAICodexTicketFailureServer, classifyOpenAICodexTicketFailure(502, nil))
	require.Equal(t, openAICodexTicketFailureServer, classifyOpenAICodexTicketFailure(503, nil))
	require.Equal(t, openAICodexTicketFailureUnexpected, classifyOpenAICodexTicketFailure(404, nil))
	require.Equal(t, openAICodexTicketFailureUnexpected, classifyOpenAICodexTicketFailure(400, nil))
}

func TestOpenAICodexTicketRetryAfter_SecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	header := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return h
	}

	require.Equal(t, 90*time.Second, openAICodexTicketRetryAfter(header("90"), now))
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(header(""), now))
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(nil, now))
	// Zero and negative second counts carry no wait; the ladder takes over.
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(header("0"), now))
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(header("-5"), now))
	// Garbage must not be silently read as a huge wait.
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(header("soon"), now))

	// HTTP-date form, relative to the supplied clock.
	future := now.Add(3 * time.Minute).Format(http.TimeFormat)
	require.Equal(t, 3*time.Minute, openAICodexTicketRetryAfter(header(future), now))
	// A date already in the past is not a wait.
	past := now.Add(-time.Hour).Format(http.TimeFormat)
	require.Equal(t, time.Duration(0), openAICodexTicketRetryAfter(header(past), now))
}

func TestOpenAICodexTicketBackoff_NetworkLadderAndCap(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := openAICodexTicketRetryState{}
	// 6, 12, 24, 48, 96, 120 then pinned at 120.
	for i, want := range []int{6, 12, 24, 48, 96, 120, 120, 120} {
		st = st.recordFailure(openAICodexTicketFailureNetwork, 0, 0, now)
		base := time.Duration(want) * time.Second
		delay := st.NextRetryAt.Sub(now)
		require.GreaterOrEqual(t, delay, base, "attempt %d must not undercut the ladder", i+1)
		// Jitter adds at most 10% (capped at 5s) and never subtracts.
		require.LessOrEqual(t, delay, base+base/10+time.Second)
		require.Equal(t, i+1, st.ConsecutiveFailures)
		now = st.NextRetryAt
	}
}

func TestOpenAICodexTicketBackoff_BadTicketFastThenSlow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := openAICodexTicketRetryState{}
	// A 200 that yields no usable ticket retries fast a few times, then backs
	// off into the 30-60s band instead of hammering the upstream.
	for _, want := range []int{5, 5, 5, 30, 45, 60, 60} {
		st = st.recordFailure(openAICodexTicketFailureBadTicket, http.StatusOK, 0, now)
		base := time.Duration(want) * time.Second
		require.GreaterOrEqual(t, st.NextRetryAt.Sub(now), base)
		now = st.NextRetryAt
	}
}

func TestOpenAICodexTicketBackoff_RateLimitedHonorsRetryAfter(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// A Retry-After longer than the ladder must be obeyed in full.
	st := openAICodexTicketRetryState{}.recordFailure(
		openAICodexTicketFailureRateLimited, http.StatusTooManyRequests, 15*time.Minute, now)
	require.GreaterOrEqual(t, st.NextRetryAt.Sub(now), 15*time.Minute)

	// A Retry-After shorter than the ladder must not shorten the ladder either:
	// the wait is the longer of the two.
	short := openAICodexTicketRetryState{}.recordFailure(
		openAICodexTicketFailureRateLimited, http.StatusTooManyRequests, time.Second, now)
	require.GreaterOrEqual(t, short.NextRetryAt.Sub(now), 60*time.Second)
}

func TestOpenAICodexTicketBackoff_ReasonChangeRestartsCount(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := openAICodexTicketRetryState{}
	st = st.recordFailure(openAICodexTicketFailureNetwork, 0, 0, now)
	st = st.recordFailure(openAICodexTicketFailureNetwork, 0, 0, now)
	require.Equal(t, 2, st.ConsecutiveFailures)

	// A different failure class is a different problem: the ladder restarts.
	st = st.recordFailure(openAICodexTicketFailureForbidden, http.StatusForbidden, 0, now)
	require.Equal(t, 1, st.ConsecutiveFailures)
	require.Equal(t, openAICodexTicketFailureForbidden, st.Reason)
	require.Equal(t, http.StatusForbidden, st.LastHTTPStatus)
	// 403 waits far longer than a network blip; we do not guess at the cause.
	require.GreaterOrEqual(t, st.NextRetryAt.Sub(now), 5*time.Minute)
}

func TestOpenAICodexTicketBackoff_SuccessResets(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st := openAICodexTicketRetryState{configFingerprint: "cfg-a"}
	st = st.recordFailure(openAICodexTicketFailureServer, 500, 0, now)
	st = st.recordFailure(openAICodexTicketFailureServer, 500, 0, now)
	require.False(t, st.dueAt(now))

	st = st.recordSuccess()
	require.Zero(t, st.ConsecutiveFailures)
	require.Empty(t, string(st.Reason))
	require.True(t, st.NextRetryAt.IsZero())
	require.True(t, st.dueAt(now))
	// The fingerprint survives so the next cycle does not read a config change.
	require.Equal(t, "cfg-a", st.configFingerprint)
}

func TestOpenAICodexTicketBackoff_ConfigChangeClearsBackoffExceptLiveRateLimit(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// A proxy misconfiguration clears the moment the config is corrected.
	proxyFail := openAICodexTicketRetryState{configFingerprint: "old"}.
		recordFailure(openAICodexTicketFailureProxyConfig, 0, 0, now)
	require.False(t, proxyFail.dueAt(now))
	recovered := proxyFail.applyConfigFingerprint("new", now)
	require.True(t, recovered.dueAt(now))
	require.Zero(t, recovered.ConsecutiveFailures)

	// A live 429 window is the upstream's, not ours: editing local config
	// must not shorten it.
	limited := openAICodexTicketRetryState{configFingerprint: "old"}.
		recordFailure(openAICodexTicketFailureRateLimited, http.StatusTooManyRequests, 10*time.Minute, now)
	kept := limited.applyConfigFingerprint("new", now)
	require.False(t, kept.dueAt(now))
	require.GreaterOrEqual(t, kept.NextRetryAt.Sub(now), 10*time.Minute)
	require.Equal(t, "new", kept.configFingerprint)

	// Once that window has elapsed, a config change resets normally.
	after := kept.NextRetryAt.Add(time.Second)
	require.True(t, kept.applyConfigFingerprint("newer", after).dueAt(after))

	// An unchanged fingerprint never disturbs an in-flight backoff.
	same := proxyFail.applyConfigFingerprint("old", now)
	require.Equal(t, proxyFail, same)
}

// ---- harvester integration ----

type codexTicketScriptedUpstream struct {
	HTTPUpstream
	mu        sync.Mutex
	calls     atomic.Int64
	inflight  atomic.Int64
	maxSeen   atomic.Int64
	responses []func() (*http.Response, error)
	hold      chan struct{}
}

func (u *codexTicketScriptedUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	cur := u.inflight.Add(1)
	for {
		prev := u.maxSeen.Load()
		if cur <= prev || u.maxSeen.CompareAndSwap(prev, cur) {
			break
		}
	}
	defer u.inflight.Add(-1)
	if u.hold != nil {
		select {
		case <-u.hold:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	idx := int(u.calls.Add(1)) - 1
	u.mu.Lock()
	defer u.mu.Unlock()
	if idx < len(u.responses) {
		return u.responses[idx]()
	}
	if len(u.responses) > 0 {
		return u.responses[len(u.responses)-1]()
	}
	return nil, io.EOF
}

func codexTicketOKResponse() (*http.Response, error) {
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func codexTicketStatusResponse(status int, retryAfter string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
}

func codexTicketHarvestService(t *testing.T, cfg config.OpenAICodexTicketConfig, upstream HTTPUpstream, accounts ...Account) (*OpenAIGatewayService, *codexTicketRefreshRepo) {
	t.Helper()
	repo := &codexTicketRefreshRepo{accounts: accounts}
	svc := ticketTestService(t, cfg, upstream)
	svc.accountRepo = repo
	return svc, repo
}

func TestRefreshOpenAICodexTickets_BacksOffAfter429AndSkipsNextCycle(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketStatusResponse(429, "900")},
	}
	svc, _ := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Models:          []string{"gpt-6-astra"},
		HarvestProxyURL: "socks5h://proxy.example.com:1080",
	}, upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(1), upstream.calls.Load())

	state := svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra"))
	require.Equal(t, openAICodexTicketFailureRateLimited, state.Reason)
	require.Equal(t, 1, state.ConsecutiveFailures)
	require.Equal(t, http.StatusTooManyRequests, state.LastHTTPStatus)
	// The upstream asked for 15 minutes; the next cycle must respect it.
	require.GreaterOrEqual(t, time.Until(state.NextRetryAt), 14*time.Minute)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(1), upstream.calls.Load(), "backoff must suppress the immediate retry")
}

func TestRefreshOpenAICodexTickets_SuccessClearsPriorFailure(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){
			codexTicketStatusResponse(500, ""),
			codexTicketOKResponse,
		},
	}
	svc, _ := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Models:          []string{"gpt-6-astra"},
		HarvestProxyURL: "socks5h://proxy.example.com:1080",
	}, upstream, *account)
	key := openAICodexTicketKey(41, "gpt-6-astra")

	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, 1, svc.openAICodexTicketRetrySnapshot(key).ConsecutiveFailures)

	// Fast-forward past the 6s first rung rather than sleeping.
	svc.openAICodexTicketRetryMutate(key, func(st openAICodexTicketRetryState) openAICodexTicketRetryState {
		st.NextRetryAt = time.Now().Add(-time.Second)
		return st
	})
	svc.refreshOpenAICodexTickets(context.Background())

	require.Equal(t, int64(2), upstream.calls.Load())
	require.Zero(t, svc.openAICodexTicketRetrySnapshot(key).ConsecutiveFailures)
	require.True(t, svc.lookupOpenAICodexTicket(&account, "gpt-6-astra").valid(time.Now(), 292))
}

func TestRefreshOpenAICodexTickets_MissingProxySkipsRequestAndRecoversOnFix(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	svc, _ := codexTicketHarvestService(t, cfg, upstream, *account)
	key := openAICodexTicketKey(41, "gpt-6-astra")

	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, upstream.calls.Load(), "an unset proxy must never produce an empty request")
	state := svc.openAICodexTicketRetrySnapshot(key)
	require.Equal(t, openAICodexTicketFailureProxyConfig, state.Reason)
	require.False(t, state.dueAt(time.Now()))

	// Correcting the proxy changes the config fingerprint, which clears the
	// backoff on the very next cycle instead of waiting it out.
	svc.cfg.Gateway.OpenAICodexTicket.HarvestProxyURL = "socks5h://proxy.example.com:1080"
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(1), upstream.calls.Load())
	require.Zero(t, svc.openAICodexTicketRetrySnapshot(key).ConsecutiveFailures)
}

func TestRefreshOpenAICodexTickets_MalformedProxySkipsRequest(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Models:          []string{"gpt-6-astra"},
		HarvestProxyURL: "ftp://nope/path?x=1",
	}, upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, upstream.calls.Load())
	require.Equal(t, openAICodexTicketFailureProxyConfig,
		svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra")).Reason)
}

func TestRefreshOpenAICodexTickets_RespectsGlobalConcurrencyBound(t *testing.T) {
	accounts := make([]Account, 0, 6)
	for id := int64(1); id <= 6; id++ {
		acc := ticketTestAccount(id)
		acc.Status = StatusActive
		accounts = append(accounts, *acc)
	}
	// Hold every probe open until all admitted ones are in flight, so the peak
	// count is observable rather than racing to completion one at a time.
	upstream := &codexTicketScriptedUpstream{
		hold:      make(chan struct{}),
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:               true,
		Models:                []string{"gpt-6-astra", "gpt-5.6-sol"},
		HarvestProxyURL:       "socks5h://proxy.example.com:1080",
		HarvestMaxConcurrency: 3,
	}, upstream, accounts...)

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.refreshOpenAICodexTickets(context.Background())
	}()

	require.Eventually(t, func() bool { return upstream.inflight.Load() == 3 }, 5*time.Second, 5*time.Millisecond)
	// Give any unbounded spillover a chance to show up before releasing.
	time.Sleep(50 * time.Millisecond)
	require.LessOrEqual(t, upstream.maxSeen.Load(), int64(3))
	close(upstream.hold)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh cycle did not drain")
	}
	// 6 accounts x 2 models all get probed, just never more than 3 at a time.
	require.Equal(t, int64(12), upstream.calls.Load())
	require.LessOrEqual(t, upstream.maxSeen.Load(), int64(3))
}

func TestRefreshOpenAICodexTickets_CancellationDoesNotCountAsFailure(t *testing.T) {
	accounts := make([]Account, 0, 4)
	for id := int64(1); id <= 4; id++ {
		acc := ticketTestAccount(id)
		acc.Status = StatusActive
		accounts = append(accounts, *acc)
	}
	upstream := &codexTicketScriptedUpstream{
		hold:      make(chan struct{}),
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:               true,
		Models:                []string{"gpt-6-astra"},
		HarvestProxyURL:       "socks5h://proxy.example.com:1080",
		HarvestMaxConcurrency: 2,
	}, upstream, accounts...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.refreshOpenAICodexTickets(ctx)
	}()
	require.Eventually(t, func() bool { return upstream.inflight.Load() == 2 }, 5*time.Second, 5*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled cycle leaked a goroutine")
	}

	// Shutdown is not an account problem: nothing may be penalized for it.
	for id := int64(1); id <= 4; id++ {
		state := svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(id, "gpt-6-astra"))
		require.Zero(t, state.ConsecutiveFailures, "account %d was penalized for a cancellation", id)
	}
}

func TestRefreshOpenAICodexTickets_KeepsValidTicketAcrossFailures(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){
			codexTicketOKResponse,
			codexTicketStatusResponse(500, ""),
		},
	}
	cfg := config.OpenAICodexTicketConfig{
		Enabled:              true,
		Models:               []string{"gpt-6-astra"},
		HarvestProxyURL:      "socks5h://proxy.example.com:1080",
		TTLSeconds:           3600,
		RefreshBeforeSeconds: 600,
	}
	svc, _ := codexTicketHarvestService(t, cfg, upstream, *account)
	key := openAICodexTicketKey(41, "gpt-6-astra")

	svc.refreshOpenAICodexTickets(context.Background())
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	state := ticket.State

	// Push the ticket into its refresh window so the next cycle re-probes it,
	// and make that probe fail.
	svc.openaiCodexTickets.Store(key, &openAICodexTicket{
		AccountID: 41, Model: "gpt-6-astra", State: state, Length: 292,
		CapturedAt: time.Now().Add(-time.Hour),
		ExpiresAt:  time.Now().Add(5 * time.Minute),
	})
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(2), upstream.calls.Load())

	// A failed refresh must not discard a ticket that is still valid.
	still := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, still)
	require.Equal(t, state, still.State)
	require.True(t, still.valid(time.Now(), 292))
	require.Equal(t, 1, svc.openAICodexTicketRetrySnapshot(key).ConsecutiveFailures)
}

func TestRefreshOpenAICodexTickets_PrunesStateForRemovedAccounts(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketStatusResponse(500, "")},
	}
	svc, repo := codexTicketHarvestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		Models:          []string{"gpt-6-astra"},
		HarvestProxyURL: "socks5h://proxy.example.com:1080",
	}, upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, 1, svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra")).ConsecutiveFailures)

	// The account goes away; its retry state must not linger in memory.
	repo.accounts = nil
	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra")).ConsecutiveFailures)
}

func TestOpenAICodexTicketStatusesForAccount_ExposesRetryStateWithoutSecrets(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketStatusResponse(429, "600")},
	}
	cfg := config.OpenAICodexTicketConfig{
		Enabled:         true,
		FailClosed:      true,
		TargetLength:    292,
		Models:          []string{"gpt-6-astra"},
		HarvestProxyURL: "socks5h://user:hunter2@proxy.example.com:1080",
	}
	svc, _ := codexTicketHarvestService(t, cfg, upstream, *account)
	svc.refreshOpenAICodexTickets(context.Background())

	got := svc.OpenAICodexTicketStatusesForAccount(account, cfg, time.Now())
	require.Len(t, got, 1)
	require.False(t, got[0].Ready)
	require.True(t, got[0].Blocked)
	require.Equal(t, "rate_limited", got[0].LastFailureReason)
	require.Equal(t, http.StatusTooManyRequests, got[0].LastFailureStatus)
	require.Equal(t, 1, got[0].ConsecutiveFailures)
	require.NotNil(t, got[0].NextRetryAt)
	require.Greater(t, got[0].RetryInSeconds, int64(9*60))

	// The summary must never leak ticket material or proxy credentials.
	require.NotContains(t, got[0].LastFailureReason, "hunter2")
	require.NotContains(t, got[0].LastFailureReason, "proxy.example.com")
	require.Zero(t, got[0].Length)
}

func TestOpenAICodexTicketStatusesForAccount_NilGatewayFallsBackToPlainStatuses(t *testing.T) {
	var svc *OpenAIGatewayService
	account := ticketTestAccount(41)
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}
	got := svc.OpenAICodexTicketStatusesForAccount(account, cfg, time.Now())
	require.Len(t, got, 1)
	require.Empty(t, got[0].LastFailureReason)
	require.Zero(t, got[0].ConsecutiveFailures)
}

func TestOpenAICodexTicketConfigFingerprint_MasksProxyCredentials(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 292, HarvestAttemptTimeoutSeconds: 25}
	fp := openAICodexTicketConfigFingerprint("socks5h://user:hunter2@proxy.example.com:1080", cfg)
	require.NotContains(t, fp, "hunter2")

	// A password change is still a config change the fingerprint must notice.
	other := openAICodexTicketConfigFingerprint("socks5h://user:other@proxy.example.com:1080", cfg)
	require.Equal(t, fp, other, "masked passwords collapse; host changes are what matter")
	require.NotEqual(t, fp, openAICodexTicketConfigFingerprint("socks5h://elsewhere:1080", cfg))
}

func TestOpenAICodexTicketProxyUnsetErrorIsDistinct(t *testing.T) {
	require.True(t, errors.Is(errOpenAICodexTicketProxyUnset, errOpenAICodexTicketProxyUnset))
	require.NotContains(t, errOpenAICodexTicketProxyUnset.Error(), "://")
}
