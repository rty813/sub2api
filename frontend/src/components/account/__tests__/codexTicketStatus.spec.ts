import { describe, expect, it } from 'vitest'
import type { ComposerTranslation } from 'vue-i18n'
import {
  codexTicketCooldownLabel,
  codexTicketCooldownReasonLabel,
  codexTicketFailureDetail,
  codexTicketReasonLabel,
  codexTicketRetryLabel,
  formatCodexTicketDuration,
} from '../codexTicketStatus'

// Echo the key plus its params so assertions can see what was interpolated.
const t = ((key: string, params?: Record<string, unknown>) =>
  params ? `${key}(${JSON.stringify(params)})` : key) as unknown as ComposerTranslation

describe('formatCodexTicketDuration', () => {
  it.each([
    [0, '0m00s'],
    [9, '0m09s'],
    [90, '1m30s'],
    [3600, '60m00s'],
  ])('formats %i seconds as %s', (seconds, expected) => {
    expect(formatCodexTicketDuration(seconds)).toBe(expected)
  })

  it('clamps missing and negative values instead of rendering NaN', () => {
    expect(formatCodexTicketDuration(undefined)).toBe('0m00s')
    expect(formatCodexTicketDuration(-30)).toBe('0m00s')
  })
})

describe('codexTicketReasonLabel', () => {
  it('translates every reason the backend can emit', () => {
    const reasons = [
      'network',
      'server',
      'rate_limited',
      'unauthorized',
      'forbidden',
      'token',
      'invalid_ticket',
      'proxy_config',
      'unexpected_status',
    ]
    for (const reason of reasons) {
      expect(codexTicketReasonLabel(t, reason)).toBe(
        `admin.accounts.openai.codexTurnTicketReason.${reason}`,
      )
    }
  })

  it('shows an unrecognised reason verbatim rather than dropping it', () => {
    // A backend that grows a new failure class must still be readable here.
    expect(codexTicketReasonLabel(t, 'some_new_class')).toBe('some_new_class')
  })

  it('renders nothing when there is no reason', () => {
    expect(codexTicketReasonLabel(t, undefined)).toBe('')
    expect(codexTicketReasonLabel(t, '   ')).toBe('')
  })
})

describe('codexTicketRetryLabel', () => {
  it('stays empty until there is a failure to report', () => {
    expect(codexTicketRetryLabel(t, {})).toBe('')
    expect(codexTicketRetryLabel(t, { consecutive_failures: 0, retry_in_seconds: 42 })).toBe('')
  })

  it('renders the remaining wait', () => {
    expect(codexTicketRetryLabel(t, { consecutive_failures: 2, retry_in_seconds: 90 })).toBe(
      'admin.accounts.openai.codexTurnTicketRetry({"time":"1m30s"})',
    )
  })

  it('says the retry is due once the window has elapsed', () => {
    // retry_in_seconds is a server-computed remainder, so it hits 0 between polls.
    expect(codexTicketRetryLabel(t, { consecutive_failures: 2, retry_in_seconds: 0 })).toBe(
      'admin.accounts.openai.codexTurnTicketRetryDue',
    )
    expect(codexTicketRetryLabel(t, { consecutive_failures: 2 })).toBe(
      'admin.accounts.openai.codexTurnTicketRetryDue',
    )
  })
})

describe('codexTicketFailureDetail', () => {
  it('stays empty without a failure', () => {
    expect(codexTicketFailureDetail(t, { ready: true })).toBe('')
  })

  it('combines reason, count and wait', () => {
    expect(
      codexTicketFailureDetail(t, {
        last_failure_reason: 'rate_limited',
        last_failure_status: 429,
        consecutive_failures: 3,
        retry_in_seconds: 600,
      }),
    ).toBe(
      'admin.accounts.openai.codexTurnTicketFailureDetail(' +
        JSON.stringify({
          reason: 'admin.accounts.openai.codexTurnTicketReason.rate_limited',
          count: 3,
          time: '10m00s',
        }) +
        ')',
    )
  })

  it('substitutes the due label when the wait has elapsed', () => {
    const detail = codexTicketFailureDetail(t, {
      last_failure_reason: 'network',
      consecutive_failures: 1,
      retry_in_seconds: 0,
    })
    expect(detail).toContain('admin.accounts.openai.codexTurnTicketRetryDue')
  })
})

describe('codexTicketCooldownReasonLabel', () => {
  it('translates every cooldown class the backend can emit', () => {
    const reasons = [
      'rate_limited',
      'overloaded',
      'temp_unschedulable',
      'runtime_block',
      'model_breaker',
    ]
    for (const reason of reasons) {
      expect(codexTicketCooldownReasonLabel(t, reason)).toBe(
        `admin.accounts.openai.codexTurnTicketCooldownReason.${reason}`,
      )
    }
  })

  it('shows an unrecognised cooldown class verbatim', () => {
    expect(codexTicketCooldownReasonLabel(t, 'brand_new')).toBe('brand_new')
    expect(codexTicketCooldownReasonLabel(t, undefined)).toBe('')
  })
})

describe('codexTicketCooldownLabel', () => {
  it('stays empty unless harvesting is actually paused', () => {
    expect(codexTicketCooldownLabel(t, {})).toBe('')
    expect(codexTicketCooldownLabel(t, { cooldown_reason: 'rate_limited' })).toBe('')
  })

  it('names the scope and the remaining wait', () => {
    expect(
      codexTicketCooldownLabel(t, {
        harvest_paused: true,
        cooldown_scope: 'account',
        cooldown_reason: 'rate_limited',
        cooldown_in_seconds: 250,
      }),
    ).toBe(
      'admin.accounts.openai.codexTurnTicketCooldownIn(' +
        JSON.stringify({
          scope: 'admin.accounts.openai.codexTurnTicketCooldownScopeAccount',
          reason: 'admin.accounts.openai.codexTurnTicketCooldownReason.rate_limited',
          time: '4m10s',
        }) +
        ')',
    )
  })

  it('distinguishes a model-scoped cooldown from an account-wide one', () => {
    const label = codexTicketCooldownLabel(t, {
      harvest_paused: true,
      cooldown_scope: 'model',
      cooldown_reason: 'model_breaker',
      cooldown_in_seconds: 45,
    })
    expect(label).toContain('codexTurnTicketCooldownScopeModel')
    expect(label).not.toContain('codexTurnTicketCooldownScopeAccount')
  })

  it('still reports the pause when the reset time is unknown', () => {
    // A runtime block can be active without a readable deadline; "waiting" beats
    // silently rendering "resumes in 0m00s".
    expect(
      codexTicketCooldownLabel(t, {
        harvest_paused: true,
        cooldown_scope: 'account',
        cooldown_reason: 'runtime_block',
      }),
    ).toBe(
      'admin.accounts.openai.codexTurnTicketCooldown(' +
        JSON.stringify({
          scope: 'admin.accounts.openai.codexTurnTicketCooldownScopeAccount',
          reason: 'admin.accounts.openai.codexTurnTicketCooldownReason.runtime_block',
        }) +
        ')',
    )
  })
})

describe('cooldown outranks backoff', () => {
  const paused = {
    harvest_paused: true,
    cooldown_scope: 'account',
    cooldown_reason: 'rate_limited',
    cooldown_in_seconds: 120,
  }

  it('reports the wait rather than a retry countdown', () => {
    // Nothing is being sent at all, so a "retry in 12s" line would be a lie.
    expect(codexTicketRetryLabel(t, { ...paused, consecutive_failures: 3, retry_in_seconds: 12 }))
      .toContain('codexTurnTicketCooldownIn')
    expect(codexTicketFailureDetail(t, { ...paused, consecutive_failures: 3, retry_in_seconds: 12 }))
      .toContain('codexTurnTicketCooldownIn')
  })

  it('shows the pause even with no failure history at all', () => {
    // A pure skip never increments the counter, so this is the common case.
    expect(codexTicketRetryLabel(t, paused)).toContain('codexTurnTicketCooldownIn')
    expect(codexTicketFailureDetail(t, paused)).toContain('codexTurnTicketCooldownIn')
  })
})
