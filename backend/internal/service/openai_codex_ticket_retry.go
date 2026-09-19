package service

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 采票退避状态只存在内存里：它描述的是「下一次该什么时候再打」，是一个纯粹的
// 运行期调度提示，不是业务数据。刻意不落库，原因有三：
//   - 数据库已经 19GB，按账号×模型每次失败都写一行会变成高频全表写；
//   - 重启后上游状况通常已经变化，沿用旧的 120 秒退避反而拖慢恢复；
//   - 门票本身（account.Extra）才是需要持久化的东西，已经在落库了。
//
// 重启行为：退避状态清空，所有缺票的账号×模型在第一个周期立即重试一次；
// 若上游仍在限流，第一次 429 会重新读回 Retry-After，不会比重启前更激进。
type openAICodexTicketFailureReason string

const (
	openAICodexTicketFailureNone        openAICodexTicketFailureReason = ""
	openAICodexTicketFailureProxyConfig openAICodexTicketFailureReason = "proxy_config"
	openAICodexTicketFailureToken       openAICodexTicketFailureReason = "token"
	openAICodexTicketFailureNetwork     openAICodexTicketFailureReason = "network"
	openAICodexTicketFailureServer      openAICodexTicketFailureReason = "server"
	openAICodexTicketFailureRateLimited openAICodexTicketFailureReason = "rate_limited"
	openAICodexTicketFailureUnauthed    openAICodexTicketFailureReason = "unauthorized"
	openAICodexTicketFailureForbidden   openAICodexTicketFailureReason = "forbidden"
	openAICodexTicketFailureBadTicket   openAICodexTicketFailureReason = "invalid_ticket"
	openAICodexTicketFailureUnexpected  openAICodexTicketFailureReason = "unexpected_status"
)

// 每一类失败的退避阶梯（秒）。索引 = 连续失败次数-1，超出则取最后一档。
// 阶梯只描述「这类故障多久后重试才不算白打」，与是否启用无关。
var openAICodexTicketBackoffLadders = map[openAICodexTicketFailureReason][]int{
	// 网络抖动 / 5xx：题面要求的 6,12,24,48,96,120。
	openAICodexTicketFailureNetwork:    {6, 12, 24, 48, 96, 120},
	openAICodexTicketFailureServer:     {6, 12, 24, 48, 96, 120},
	openAICodexTicketFailureUnexpected: {6, 12, 24, 48, 96, 120},
	// 200 但票不合格：多半是瞬时的，先快速重试几次再拉到 30~60 秒。
	openAICodexTicketFailureBadTicket: {5, 5, 5, 30, 45, 60},
	// 429：至少遵守 Retry-After；没有该头时按这个阶梯兜底。
	openAICodexTicketFailureRateLimited: {60, 120, 300, 600},
	// 401 / 取不到 token：刷新逻辑在 GetAccessToken 内部，这里只负责冷却防刷。
	openAICodexTicketFailureUnauthed: {15, 30, 60, 120, 300},
	openAICodexTicketFailureToken:    {15, 30, 60, 120, 300},
	// 403 成因不明（风控、地区、账号状态都可能），等久一点，不猜也不试探。
	openAICodexTicketFailureForbidden: {300, 600, 900, 1800},
	// 代理没配或配错：根本不发请求，等人改配置；改完由 fingerprint 立即解除。
	openAICodexTicketFailureProxyConfig: {30, 60, 120, 300},
}

// openAICodexTicketRetryState 是单个「账号 × 模型」的采票重试状态。
// 值语义：调用方总是拿到副本，不会和 harvester 的写入竞争。
type openAICodexTicketRetryState struct {
	Reason               openAICodexTicketFailureReason
	ConsecutiveFailures  int
	LastHTTPStatus       int
	LastFailureAt        time.Time
	NextRetryAt          time.Time
	configFingerprint    string
	rateLimitDeadlineSet bool
}

// dueAt 判断当前是否可以再打一发。零值状态（刚启动、刚成功）总是可打。
func (st openAICodexTicketRetryState) dueAt(now time.Time) bool {
	return st.NextRetryAt.IsZero() || !now.Before(st.NextRetryAt)
}

// classifyOpenAICodexTicketFailure 把一次探测结果归类。
// err != nil 一律算网络类：这条路径只有传输/超时错误，业务错误都带状态码回来。
func classifyOpenAICodexTicketFailure(status int, err error) openAICodexTicketFailureReason {
	if err != nil {
		return openAICodexTicketFailureNetwork
	}
	switch {
	case status == http.StatusOK:
		// 200 但没拿到合格的 292：可能是空头、长度不对或前缀不对，成因一致，
		// 都按「这一发没打出票」处理。
		return openAICodexTicketFailureBadTicket
	case status == http.StatusTooManyRequests:
		return openAICodexTicketFailureRateLimited
	case status == http.StatusUnauthorized:
		return openAICodexTicketFailureUnauthed
	case status == http.StatusForbidden:
		return openAICodexTicketFailureForbidden
	case status >= 500:
		return openAICodexTicketFailureServer
	default:
		return openAICodexTicketFailureUnexpected
	}
}

// openAICodexTicketRetryAfter 解析 Retry-After，兼容秒数与 HTTP 日期两种写法。
// 无法解析或已过期返回 0，交给阶梯兜底。
func openAICodexTicketRetryAfter(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return 0
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

// openAICodexTicketLadderDelay 取该类故障在第 attempt 次连续失败时的基础间隔。
func openAICodexTicketLadderDelay(reason openAICodexTicketFailureReason, attempt int) time.Duration {
	ladder := openAICodexTicketBackoffLadders[reason]
	if len(ladder) == 0 {
		ladder = openAICodexTicketBackoffLadders[openAICodexTicketFailureNetwork]
	}
	if attempt < 1 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(ladder) {
		idx = len(ladder) - 1
	}
	return time.Duration(ladder[idx]) * time.Second
}

// openAICodexTicketJitter 只加不减：抖动用来打散账号间的同步重试，
// 绝不能把上游明确要求的等待时间缩短。上限 10% 且不超过 5 秒。
func openAICodexTicketJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	span := delay / 10
	if span > 5*time.Second {
		span = 5 * time.Second
	}
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(span) + 1))
}

// recordFailure 推进退避状态。reason 变化时连续计数重新起算，
// 因为「网络不通 3 次」和「被限流 1 次」不是同一个故障。
func (st openAICodexTicketRetryState) recordFailure(
	reason openAICodexTicketFailureReason,
	status int,
	retryAfter time.Duration,
	now time.Time,
) openAICodexTicketRetryState {
	if st.Reason != reason {
		st.ConsecutiveFailures = 0
	}
	st.Reason = reason
	st.ConsecutiveFailures++
	st.LastHTTPStatus = status
	st.LastFailureAt = now

	delay := openAICodexTicketLadderDelay(reason, st.ConsecutiveFailures)
	st.rateLimitDeadlineSet = false
	if reason == openAICodexTicketFailureRateLimited && retryAfter > 0 {
		// 上游明确给了等待时间：这是硬下界，阶梯只能比它更久，不能更短。
		if retryAfter > delay {
			delay = retryAfter
		}
		st.rateLimitDeadlineSet = true
	}
	next := now.Add(delay + openAICodexTicketJitter(delay))
	// 同一轮里若已有更晚的截止时间（例如上一发 429 要求等更久），不提前。
	if next.After(st.NextRetryAt) {
		st.NextRetryAt = next
	}
	return st
}

// recordSuccess 清空退避，但保留 configFingerprint 以免下个周期误判为配置变更。
func (st openAICodexTicketRetryState) recordSuccess() openAICodexTicketRetryState {
	fingerprint := st.configFingerprint
	return openAICodexTicketRetryState{configFingerprint: fingerprint}
}

// applyConfigFingerprint 在采票配置（代理）变更时立刻解除退避，让修正后的配置
// 尽快生效。唯一的例外是仍在有效期内的 429：上游的限流窗口不会因为我们改了
// 本地配置就消失，缩短它只会换来更长的封禁。
func (st openAICodexTicketRetryState) applyConfigFingerprint(fingerprint string, now time.Time) openAICodexTicketRetryState {
	if st.configFingerprint == fingerprint {
		return st
	}
	if st.rateLimitDeadlineSet && now.Before(st.NextRetryAt) {
		st.configFingerprint = fingerprint
		return st
	}
	return openAICodexTicketRetryState{configFingerprint: fingerprint}
}

// openAICodexTicketRetrySnapshot 读取某个 key 的重试状态副本。
func (s *OpenAIGatewayService) openAICodexTicketRetrySnapshot(key string) openAICodexTicketRetryState {
	if s == nil {
		return openAICodexTicketRetryState{}
	}
	s.openaiCodexTicketRetryMu.Lock()
	defer s.openaiCodexTicketRetryMu.Unlock()
	return s.openaiCodexTicketRetry[key]
}

// openAICodexTicketRetryMutate 在锁内读改写，避免两个模型的 goroutine 互相覆盖。
func (s *OpenAIGatewayService) openAICodexTicketRetryMutate(
	key string,
	mutate func(openAICodexTicketRetryState) openAICodexTicketRetryState,
) openAICodexTicketRetryState {
	if s == nil || mutate == nil {
		return openAICodexTicketRetryState{}
	}
	s.openaiCodexTicketRetryMu.Lock()
	defer s.openaiCodexTicketRetryMu.Unlock()
	if s.openaiCodexTicketRetry == nil {
		s.openaiCodexTicketRetry = make(map[string]openAICodexTicketRetryState)
	}
	next := mutate(s.openaiCodexTicketRetry[key])
	if next == (openAICodexTicketRetryState{}) {
		delete(s.openaiCodexTicketRetry, key)
		return next
	}
	s.openaiCodexTicketRetry[key] = next
	return next
}

// pruneOpenAICodexTicketRetry 丢弃已不存在（停用/删除）的账号×模型状态，
// 防止这张内存表随账号变更无界增长。
func (s *OpenAIGatewayService) pruneOpenAICodexTicketRetry(live map[string]struct{}) {
	if s == nil {
		return
	}
	s.openaiCodexTicketRetryMu.Lock()
	defer s.openaiCodexTicketRetryMu.Unlock()
	for key := range s.openaiCodexTicketRetry {
		if _, ok := live[key]; !ok {
			delete(s.openaiCodexTicketRetry, key)
		}
	}
}

// errOpenAICodexTicketProxyUnset 表示采票代理没配置；不是故障，是没开工。
var errOpenAICodexTicketProxyUnset = errors.New("codex ticket harvest proxy is not configured")
