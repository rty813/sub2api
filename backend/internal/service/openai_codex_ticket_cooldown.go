package service

import (
	"context"
	"time"
)

// 采票冷却门控：判断「这个账号 × 这个模型现在还能不能向上游打票」。
//
// 采票本身就是一次真实的上游请求。业务请求已经因为限流而避开的账号，后台却继续
// 按 6 秒一轮打票，只会换来更多 429、更长的封禁和白烧的代理流量。
//
// 刻意不纳入的两项：
//   - 门票门控（openAICodexTicketBlocksAccount / isOpenAIAccountRequestRuntimeBlocked）。
//     缺票正是采票要去解决的问题，把它当成禁止采票会形成
//     缺票 → 不采票 → 永远缺票 的死锁。所以这里只看限流/冷却，绝不调用那条聚合判定。
//   - 手动 Schedulable=false。那是运维对「业务流量」的编排决定，不是上游在限流；
//     保持采票可以让账号恢复调度的瞬间就有票可用，代价只是每模型每小时一发探测。
//     真要停掉某个账号的一切出站，把账号置为非 active 即可（外层已按 StatusActive 过滤）。
//
// 跳过不是失败：不进退避阶梯、不累加连续失败次数。冷却到期后下一轮自然恢复采票。
type openAICodexTicketCooldownScope string

const (
	// openAICodexTicketCooldownScopeAccount 覆盖该账号的全部模型。
	openAICodexTicketCooldownScopeAccount openAICodexTicketCooldownScope = "account"
	// openAICodexTicketCooldownScopeModel 只覆盖命中的那个模型，不误伤同账号其他模型。
	openAICodexTicketCooldownScopeModel openAICodexTicketCooldownScope = "model"
)

const (
	// openAICodexTicketCooldownRateLimited 是上游 429 写下的限流窗口（reset_at）。
	openAICodexTicketCooldownRateLimited = "rate_limited"
	// openAICodexTicketCooldownOverloaded 是上游过载窗口。
	openAICodexTicketCooldownOverloaded = "overloaded"
	// openAICodexTicketCooldownTempUnsched 是鉴权失败 / token 刷新耗尽 / 传输故障
	// 写下的临时不可调度窗口：这些故障对采票同样成立（共用凭据与代理）。
	openAICodexTicketCooldownTempUnsched = "temp_unschedulable"
	// openAICodexTicketCooldownRuntimeBlock 是进程内的账号级封禁（429 桥接、
	// access_state、upstream_disable、transport_error 等）。
	openAICodexTicketCooldownRuntimeBlock = "runtime_block"
	// openAICodexTicketCooldownModelBreaker 是模型级熔断冷却（连续失败触发，10~45 秒）。
	openAICodexTicketCooldownModelBreaker = "model_breaker"
)

// openAICodexTicketCooldownState 描述一次跳过判定。Until 为零值表示「确实在冷却，
// 但拿不到确切恢复时刻」——展示层按「等待中」处理即可，不要当成已恢复。
type openAICodexTicketCooldownState struct {
	Active bool
	Scope  openAICodexTicketCooldownScope
	Reason string
	Until  time.Time
}

// remainingAt 返回距恢复还有多久；未知恢复时刻或已到期返回 0。
func (c openAICodexTicketCooldownState) remainingAt(now time.Time) time.Duration {
	if !c.Active || c.Until.IsZero() {
		return 0
	}
	if remaining := c.Until.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// openAICodexTicketCooldownFor 解析该账号×模型当前的限流/冷却状态。
// 先看账号级（影响全部模型），再看模型级（只影响该模型）。
//
// account 必须是本轮从仓储读出的快照：RateLimitResetAt / OverloadUntil /
// TempUnschedulableUntil / Extra[model_rate_limits] 都读自它。进程内的
// runtime block 与模型熔断是实时的，所以排队等待期间新产生的限流，
// 在真正发请求前的复查里也能被读到。
func (s *OpenAIGatewayService) openAICodexTicketCooldownFor(
	ctx context.Context,
	account *Account,
	model string,
	now time.Time,
) openAICodexTicketCooldownState {
	if s == nil || account == nil {
		return openAICodexTicketCooldownState{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}

	// —— 账号级：命中即停掉该账号的所有模型 ——
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return openAICodexTicketCooldownState{
			Active: true,
			Scope:  openAICodexTicketCooldownScopeAccount,
			Reason: openAICodexTicketCooldownRateLimited,
			Until:  *account.RateLimitResetAt,
		}
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return openAICodexTicketCooldownState{
			Active: true,
			Scope:  openAICodexTicketCooldownScopeAccount,
			Reason: openAICodexTicketCooldownOverloaded,
			Until:  *account.OverloadUntil,
		}
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return openAICodexTicketCooldownState{
			Active: true,
			Scope:  openAICodexTicketCooldownScopeAccount,
			Reason: openAICodexTicketCooldownTempUnsched,
			Until:  *account.TempUnschedulableUntil,
		}
	}
	// peek 只读进程内封禁，不含门票门控；顺带清掉已过期的条目。
	if snapshot := s.peekOpenAIAccountRuntimeBlock(account); snapshot.blocked {
		return openAICodexTicketCooldownState{
			Active: true,
			Scope:  openAICodexTicketCooldownScopeAccount,
			Reason: openAICodexTicketCooldownRuntimeBlock,
			Until:  snapshot.until,
		}
	}

	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return openAICodexTicketCooldownState{}
	}

	// —— 模型级：只停这个模型 ——
	// 复用调度侧同一套 scope key（含家族级 scope），保证「业务记下的限流」与
	// 「采票读到的限流」口径一致。
	if remaining := account.GetModelRateLimitRemainingTimeWithContext(ctx, model); remaining > 0 {
		return openAICodexTicketCooldownState{
			Active: true,
			Scope:  openAICodexTicketCooldownScopeModel,
			Reason: openAICodexTicketCooldownRateLimited,
			Until:  now.Add(remaining),
		}
	}
	if state := s.getOpenAIAccountModelTransientState(); state != nil {
		canonical := canonicalOpenAIAccountSchedulingModel(account, model)
		if until := state.blockedUntil(account.ID, openAIAccountModelTransientModel(canonical), now); !until.IsZero() {
			return openAICodexTicketCooldownState{
				Active: true,
				Scope:  openAICodexTicketCooldownScopeModel,
				Reason: openAICodexTicketCooldownModelBreaker,
				Until:  until,
			}
		}
	}
	return openAICodexTicketCooldownState{}
}
