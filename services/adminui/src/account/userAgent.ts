/**
 * A user agent string turned into a short, readable device summary.
 *
 * The account page's sessions table exists so a person can recognise a session
 * that is not theirs; a raw UA string is the opposite of that, hundreds of
 * characters of engine versions with the two words that matter buried in it. This
 * keeps only those: the browser and the platform.
 *
 * It is presentation only. Nothing branches on the result, and an unrecognised
 * string is shown as itself (truncated) rather than guessed at, so a new browser
 * degrades to something honest instead of to "Chrome".
 */

function browserOf(ua: string): string {
  // Order matters: Edge and Opera both carry `Chrome/`, and Chrome carries
  // `Safari/`, so the most specific token is tested first.
  if (/Edg[A-Za-z]*\//.test(ua)) return 'Edge'
  if (/OPR\//.test(ua)) return 'Opera'
  if (/Firefox\//.test(ua)) return 'Firefox'
  if (/Chrome\//.test(ua) || /CriOS\//.test(ua)) return 'Chrome'
  if (/Safari\//.test(ua)) return 'Safari'
  return ''
}

function platformOf(ua: string): string {
  if (/iPhone|iPad|iPod/.test(ua)) return 'iOS'
  if (/Android/.test(ua)) return 'Android'
  if (/Windows/.test(ua)) return 'Windows'
  if (/Mac OS X|Macintosh/.test(ua)) return 'macOS'
  if (/CrOS/.test(ua)) return 'ChromeOS'
  if (/Linux/.test(ua)) return 'Linux'
  return ''
}

export function describeUserAgent(ua: string): string {
  const trimmed = ua.trim()
  if (trimmed === '') return 'Unknown device'

  const browser = browserOf(trimmed)
  const platform = platformOf(trimmed)
  if (browser !== '' && platform !== '') return `${browser} on ${platform}`
  if (browser !== '') return browser
  if (platform !== '') return platform
  return trimmed.length > 48 ? `${trimmed.slice(0, 48)}…` : trimmed
}
