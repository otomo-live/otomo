/**
 * A password strength meter for the onboarding form.
 *
 * It is a hint, not the policy. The server validates the password (at least 12
 * characters, not a common one, not the email address) and its 400 message is
 * what the form shows when it refuses; this only gives a person a sense of where
 * they are before they submit, so the two are allowed to disagree at the margin.
 *
 * The score is 0-4 because that is what a meter renders: four filled bars.
 */

export type StrengthScore = 0 | 1 | 2 | 3 | 4

export interface PasswordStrength {
  score: StrengthScore
  label: string
  /** Sentences to show under the meter. Empty when there is nothing to fix. */
  hints: string[]
}

const LABELS: Record<StrengthScore, string> = {
  0: 'Very weak',
  1: 'Weak',
  2: 'Fair',
  3: 'Strong',
  4: 'Very strong',
}

/** Substrings a password should not be built from. Deliberately short and obvious. */
const COMMON = [
  'password',
  'passw0rd',
  '123456',
  'qwerty',
  'letmein',
  'welcome',
  'iloveyou',
  'admin',
  'monkey',
  'dragon',
  'otomo',
]

function localPartOf(email: string): string {
  const at = email.indexOf('@')
  return (at === -1 ? email : email.slice(0, at)).toLowerCase()
}

export function passwordStrength(password: string, email = ''): PasswordStrength {
  if (password.length === 0) {
    return { score: 0, label: LABELS[0], hints: ['Enter a password of at least 12 characters.'] }
  }

  const hints: string[] = []
  let points = 0

  if (password.length >= 12) points += 1
  else hints.push('Use at least 12 characters.')
  if (password.length >= 16) points += 1

  const variety = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^A-Za-z0-9]/].filter((pattern) =>
    pattern.test(password),
  ).length
  if (variety >= 2) points += 1
  else hints.push('Mix letters with numbers or symbols.')
  if (variety >= 3) points += 1

  const lower = password.toLowerCase()
  const local = localPartOf(email)
  const hasCommon = COMMON.some((word) => lower.includes(word))
  const hasEmail = local.length > 0 && lower.includes(local)
  if (hasEmail) hints.push('Do not use your email address.')
  else if (hasCommon) hints.push('Avoid common words and patterns.')
  if (hasCommon || hasEmail) points -= 2

  const score = Math.min(4, Math.max(0, points)) as StrengthScore
  return { score, label: LABELS[score], hints }
}
