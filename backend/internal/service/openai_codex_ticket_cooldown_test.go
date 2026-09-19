package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func cooldownTicketConfig(models ...string) config.OpenAICodexTicketConfig {
	return config.OpenAICodexTicketConfig{
		Enabled:         true,
		TargetLength:    292,
		Models:          models,
		HarvestProxyURL: "socks5h://proxy.example.com:1080",
	}
}

func TestRefreshOpenAICodexTickets_AccountRateLimitStopsAllOutbound(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	resetAt := time.Now().Add(20 * time.Minute)
	account.RateLimitResetAt = &resetAt

	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, cooldownTicketConfig("gpt-6-astra", "gpt-5.6-sol"), upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())

	// 账号级限流覆盖全部模型：一发都不该出去。
	require.Zero(t, upstream.calls.Load(), "a rate-limited account must produce zero harvest requests")
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		state := svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, model))
		// 跳过不是失败：既不记原因也不累加次数，否则恢复后还要先熬完退避阶梯。
		require.Zero(t, state.ConsecutiveFailures, "model %s was penalized for a pure skip", model)
		require.Empty(t, string(state.Reason))
	}
}

func TestRefreshOpenAICodexTickets_AccountRateLimitResumesAfterWindow(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	resetAt := time.Now().Add(20 * time.Minute)
	account.RateLimitResetAt = &resetAt

	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, repo := codexTicketHarvestService(t, cooldownTicketConfig("gpt-6-astra"), upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, upstream.calls.Load())

	// 窗口到期，下一轮必须自己恢复采票，不需要任何外部触发。
	expired := time.Now().Add(-time.Second)
	repo.accounts[0].RateLimitResetAt = &expired
	svc.refreshOpenAICodexTickets(context.Background())

	require.Equal(t, int64(1), upstream.calls.Load(), "harvesting must resume once the window elapses")
	require.True(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra").valid(time.Now(), 292))
}

func TestRefreshOpenAICodexTickets_ModelRateLimitSparesOtherModels(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	now := time.Now()
	// 只有 gpt-6-astra 被限流；同账号的 gpt-5.6-sol 不该被误伤。
	setAccountModelRateLimitSnapshot(account, "gpt-6-astra", now.Add(30*time.Minute), "test", now)

	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, cooldownTicketConfig("gpt-6-astra", "gpt-5.6-sol"), upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())

	// 两个模型里只有一个出站。
	require.Equal(t, int64(1), upstream.calls.Load())
	require.Zero(t, svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra")).ConsecutiveFailures)
	require.True(t, svc.lookupOpenAICodexTicket(account, "gpt-5.6-sol").valid(time.Now(), 292),
		"an unrelated model must still be harvested")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"),
		"the rate-limited model must not have been probed")
}

func TestRefreshOpenAICodexTickets_RuntimeBlockStopsHarvestAndRecovers(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	svc, _ := codexTicketHarvestService(t, cooldownTicketConfig("gpt-6-astra"), upstream, *account)

	// 进程内账号级封禁（业务侧 429 桥接写下的那条）同样要挡住采票。
	svc.BlockAccountScheduling(account, time.Now().Add(10*time.Minute), "429")
	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, upstream.calls.Load())
	require.Zero(t, svc.openAICodexTicketRetrySnapshot(openAICodexTicketKey(41, "gpt-6-astra")).ConsecutiveFailures)

	// 封禁到期后自动恢复。BlockAccountScheduling 只会延长封禁（过去的时间会被
	// 换成默认冷却），所以这里直接改写截止时间来模拟窗口走完。
	svc.openaiAccountRuntimeBlockUntil.Store(account.ID, time.Now().Add(-time.Second))
	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(1), upstream.calls.Load())
}

func TestRefreshOpenAICodexTickets_MissingTicketNeverBlocksItsOwnHarvest(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	cfg := cooldownTicketConfig("gpt-6-astra")
	// fail_closed 下缺票会挡住业务请求——采票必须不受这条门控影响，
	// 否则就是 缺票 → 不采票 → 永远缺票 的死锁。
	cfg.FailClosed = true
	svc, _ := codexTicketHarvestService(t, cfg, upstream, *account)

	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	require.False(t, svc.openAICodexTicketCooldownFor(
		context.Background(), account, "gpt-6-astra", time.Now()).Active,
		"a missing ticket must never read as a harvest cooldown")

	svc.refreshOpenAICodexTickets(context.Background())
	require.Equal(t, int64(1), upstream.calls.Load(), "a ticket-less account must still be harvested")
	require.True(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra").valid(time.Now(), 292))
}

func TestOpenAICodexTicketCooldownFor_ScopesAndReasons(t *testing.T) {
	now := time.Now()
	svc := ticketTestService(t, cooldownTicketConfig("gpt-6-astra"), nil)

	t.Run("clean account", func(t *testing.T) {
		account := ticketTestAccount(41)
		require.False(t, svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now).Active)
	})

	t.Run("overload is account scoped", func(t *testing.T) {
		account := ticketTestAccount(42)
		until := now.Add(5 * time.Minute)
		account.OverloadUntil = &until
		got := svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now)
		require.True(t, got.Active)
		require.Equal(t, openAICodexTicketCooldownScopeAccount, got.Scope)
		require.Equal(t, openAICodexTicketCooldownOverloaded, got.Reason)
		require.Equal(t, until, got.Until)
		require.InDelta(t, float64(5*time.Minute), float64(got.remainingAt(now)), float64(time.Second))
	})

	t.Run("temp unschedulable is account scoped", func(t *testing.T) {
		account := ticketTestAccount(43)
		until := now.Add(time.Minute)
		account.TempUnschedulableUntil = &until
		got := svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now)
		require.True(t, got.Active)
		require.Equal(t, openAICodexTicketCooldownScopeAccount, got.Scope)
		require.Equal(t, openAICodexTicketCooldownTempUnsched, got.Reason)
	})

	t.Run("model rate limit is model scoped", func(t *testing.T) {
		account := ticketTestAccount(44)
		setAccountModelRateLimitSnapshot(account, "gpt-6-astra", now.Add(30*time.Minute), "test", now)
		got := svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now)
		require.True(t, got.Active)
		require.Equal(t, openAICodexTicketCooldownScopeModel, got.Scope)
		require.Equal(t, openAICodexTicketCooldownRateLimited, got.Reason)
		// 同账号其他模型不受影响。
		require.False(t, svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-5.6-sol", now).Active)
	})

	t.Run("expired windows are not cooldowns", func(t *testing.T) {
		account := ticketTestAccount(45)
		past := now.Add(-time.Second)
		account.RateLimitResetAt = &past
		account.OverloadUntil = &past
		account.TempUnschedulableUntil = &past
		setAccountModelRateLimitSnapshot(account, "gpt-6-astra", past, "test", now.Add(-time.Hour))
		require.False(t, svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now).Active)
	})

	t.Run("manual unschedulable is not a harvest cooldown", func(t *testing.T) {
		// 手动禁调度是运维对业务流量的编排，不是上游限流：
		// 继续采票，账号一恢复调度就有票可用。
		account := ticketTestAccount(46)
		account.Schedulable = false
		require.False(t, svc.openAICodexTicketCooldownFor(context.Background(), account, "gpt-6-astra", now).Active)
	})

	t.Run("nil receiver and nil account", func(t *testing.T) {
		var nilSvc *OpenAIGatewayService
		require.False(t, nilSvc.openAICodexTicketCooldownFor(context.Background(), ticketTestAccount(47), "gpt-6-astra", now).Active)
		require.False(t, svc.openAICodexTicketCooldownFor(context.Background(), nil, "gpt-6-astra", now).Active)
	})
}

func TestOpenAICodexTicketStatusesForAccount_ExposesCooldownWithoutSecrets(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	resetAt := time.Now().Add(15 * time.Minute)
	account.RateLimitResetAt = &resetAt

	upstream := &codexTicketScriptedUpstream{
		responses: []func() (*http.Response, error){codexTicketOKResponse},
	}
	cfg := cooldownTicketConfig("gpt-6-astra")
	cfg.FailClosed = true
	cfg.HarvestProxyURL = "socks5h://user:hunter2@proxy.example.com:1080"
	svc, _ := codexTicketHarvestService(t, cfg, upstream, *account)

	svc.refreshOpenAICodexTickets(context.Background())
	require.Zero(t, upstream.calls.Load())

	got := svc.OpenAICodexTicketStatusesForAccount(account, cfg, time.Now())
	require.Len(t, got, 1)
	require.True(t, got[0].HarvestPaused)
	require.Equal(t, "account", got[0].CooldownScope)
	require.Equal(t, openAICodexTicketCooldownRateLimited, got[0].CooldownReason)
	require.NotNil(t, got[0].CooldownUntil)
	require.Greater(t, got[0].CooldownInSeconds, int64(14*60))
	// 等待期间没有失败，前端不该看到失败计数。
	require.Zero(t, got[0].ConsecutiveFailures)
	require.Empty(t, got[0].LastFailureReason)
	require.NotContains(t, got[0].CooldownReason, "hunter2")
	require.NotContains(t, got[0].CooldownScope, "proxy.example.com")
}
