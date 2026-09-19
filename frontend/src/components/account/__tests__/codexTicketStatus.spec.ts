import { describe, expect, it } from 'vitest'
import type { ComposerTranslation } from 'vue-i18n'
import {
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
