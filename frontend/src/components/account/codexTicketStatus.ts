import type { ComposerTranslation } from 'vue-i18n'

/**
 * Codex 292 ticket harvest backoff state, as returned by the admin account API.
 * All fields are optional: the harvester keeps this state in memory only, so a
 * freshly restarted gateway reports a ticket with no failure history at all.
 */
export interface CodexTicketRetryState {
  ready?: boolean
  last_failure_reason?: string
  last_failure_status?: number
  consecutive_failures?: number
  retry_in_seconds?: number
  /**
   * Harvesting is paused because the account (all models) or this model is
   * inside an upstream rate-limit / cooldown window. This is a wait, not a
   * failure: consecutive_failures does not move while it is set.
   */
  harvest_paused?: boolean
  cooldown_scope?: string
  cooldown_reason?: string
  cooldown_in_seconds?: number
}

const KNOWN_REASONS = new Set([
  'network',
  'server',
  'rate_limited',
  'unauthorized',
  'forbidden',
  'token',
  'invalid_ticket',
  'proxy_config',
  'unexpected_status',
])

const KNOWN_COOLDOWN_REASONS = new Set([
  'rate_limited',
  'overloaded',
  'temp_unschedulable',
  'runtime_block',
  'model_breaker',
])

/** `90` → `1m30s`, matching the ticket-remaining formatter. */
export function formatCodexTicketDuration(seconds?: number) {
  const total = Math.max(0, Math.floor(seconds || 0))
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}m${String(s).padStart(2, '0')}s`
}

/**
 * Translate the backend's normalized failure class. Unknown values are shown
 * verbatim rather than dropped, so a newly added reason still surfaces.
 */
export function codexTicketReasonLabel(t: ComposerTranslation, reason?: string) {
  const key = (reason || '').trim()
  if (!key) return ''
  if (!KNOWN_REASONS.has(key)) return key
  return t(`admin.accounts.openai.codexTurnTicketReason.${key}`)
}

/** Short "retry in 1m30s" / "retrying shortly" suffix for the compact list cell. */
export function codexTicketRetryLabel(t: ComposerTranslation, ticket: CodexTicketRetryState) {
  // A rate-limit wait outranks the backoff line: it explains why nothing is
  // being sent at all, and it is the state the harvester is actually in.
  if (ticket.harvest_paused) return codexTicketCooldownLabel(t, ticket)
  if (!ticket.consecutive_failures) return ''
  const seconds = ticket.retry_in_seconds ?? 0
  if (seconds <= 0) return t('admin.accounts.openai.codexTurnTicketRetryDue')
  return t('admin.accounts.openai.codexTurnTicketRetry', { time: formatCodexTicketDuration(seconds) })
}

/**
 * Translate the cooldown class behind a paused harvest. Unknown values are
 * shown verbatim, same as failure reasons.
 */
export function codexTicketCooldownReasonLabel(t: ComposerTranslation, reason?: string) {
  const key = (reason || '').trim()
  if (!key) return ''
  if (!KNOWN_COOLDOWN_REASONS.has(key)) return key
  return t(`admin.accounts.openai.codexTurnTicketCooldownReason.${key}`)
}

/**
 * "Account rate limited · resumes in 4m10s" line for a harvest paused by an
 * upstream cooldown. An unknown reset time still reports the wait itself.
 */
export function codexTicketCooldownLabel(t: ComposerTranslation, ticket: CodexTicketRetryState) {
  if (!ticket.harvest_paused) return ''
  const scopeKey =
    ticket.cooldown_scope === 'model'
      ? 'admin.accounts.openai.codexTurnTicketCooldownScopeModel'
      : 'admin.accounts.openai.codexTurnTicketCooldownScopeAccount'
  const scope = t(scopeKey)
  const reason = codexTicketCooldownReasonLabel(t, ticket.cooldown_reason)
  const seconds = ticket.cooldown_in_seconds ?? 0
  if (seconds <= 0) {
    return t('admin.accounts.openai.codexTurnTicketCooldown', { scope, reason })
  }
  return t('admin.accounts.openai.codexTurnTicketCooldownIn', {
    scope,
    reason,
    time: formatCodexTicketDuration(seconds),
  })
}

/** Full "reason · N failed · retry in T" line for tooltips and the edit modal. */
export function codexTicketFailureDetail(t: ComposerTranslation, ticket: CodexTicketRetryState) {
  if (ticket.harvest_paused) return codexTicketCooldownLabel(t, ticket)
  if (!ticket.consecutive_failures) return ''
  const reason = codexTicketReasonLabel(t, ticket.last_failure_reason)
  const seconds = ticket.retry_in_seconds ?? 0
  const time =
    seconds > 0
      ? formatCodexTicketDuration(seconds)
      : t('admin.accounts.openai.codexTurnTicketRetryDue')
  return t('admin.accounts.openai.codexTurnTicketFailureDetail', {
    reason,
    count: ticket.consecutive_failures,
    time,
  })
}
