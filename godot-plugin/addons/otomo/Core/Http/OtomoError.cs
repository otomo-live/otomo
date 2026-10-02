#nullable enable
using System;

namespace OtomoSdk.Http;

/// Every failure the SDK reports (guide §2.3, §4.1). Branch on Code, log RequestId.
public sealed class OtomoError : Exception
{
    /// HTTP status, or 0 when there was no response at all (network, timeout, local failure).
    public int Status { get; }

    /// Machine-readable code from the server ("expired", "invalid_token", …) or a local one:
    /// "network", "timeout", "bad_response", "unknown".
    public string Code { get; }

    /// Server request id (body, else X-Request-Id header). Empty for local failures.
    public string RequestId { get; }

    public OtomoError(int status, string code, string message, string requestId, Exception? inner = null)
        : base(message, inner)
    {
        Status = status;
        Code = code;
        RequestId = requestId;
    }

    /// 429, 5xx and "no response": worth retrying later with back-off (guide §4.5).
    public bool IsRetryableLater => Status == 0 || Status == 429 || Status >= 500;

    public override string ToString() =>
        $"OtomoError {Status} {Code}: {Message}" + (RequestId.Length > 0 ? $" (request_id {RequestId})" : "");
}
