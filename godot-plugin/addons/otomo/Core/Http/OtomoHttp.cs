#nullable enable
using System;
using System.Collections.Generic;
using System.Net;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Http;

/// Shared HTTP transport for every SDK client (guide §3.2, §4). Owns no state beyond the
/// injected HttpClient, the config and the two auth hooks. Raises AuthenticationLost when an
/// authenticated call cannot be recovered by refreshing the access token once.
public sealed class OtomoHttp
{
    private readonly HttpClient _http;
    private readonly OtomoConfig _config;

    public OtomoHttp(HttpClient http, OtomoConfig config)
    {
        _http = http ?? throw new ArgumentNullException(nameof(http));
        _config = config ?? throw new ArgumentNullException(nameof(config));
    }

    /// Current access token, or null for anonymous calls.
    public Func<string?> GetAccessToken { get; set; } = () => null;

    /// Refresh the access token once. Returns false when no usable login could be obtained.
    public Func<CancellationToken, Task<bool>> RefreshAccessToken { get; set; } = _ => Task.FromResult(false);

    /// Raised when an authenticated request still gets 401 after the single refresh+retry,
    /// or when the refresh hook returned false.
    public event Action<OtomoError>? AuthenticationLost;

    /// D5: re-run Patch once on a 409 release_outdated (guide §7.5, doc 13). Returns false
    /// when nothing useful can be retried (e.g. the client turned out to be ClientTooOld),
    /// which is treated the same as a refresh that made no difference: the original error
    /// is returned to the caller rather than retrying pointlessly.
    public Func<CancellationToken, Task<bool>> RefreshRelease { get; set; } = _ => Task.FromResult(false);

    /// Added to every request. Seam for a future release_id header.
    public IDictionary<string, string> ExtraHeaders { get; } = new Dictionary<string, string>();

    /// Deserialization options shared by the whole SDK.
    public static JsonSerializerOptions Json { get; } = new JsonSerializerOptions
    {
        PropertyNameCaseInsensitive = true,
    };

    public async Task<T?> SendAsync<T>(
        HttpMethod method,
        string pathOrUrl,
        object? body = null,
        bool authenticated = true,
        TimeSpan? timeout = null,
        CancellationToken ct = default)
    {
        using var res = await SendCoreAsync(method, pathOrUrl, body, authenticated, timeout, ct);

        if (!res.IsSuccessStatusCode)
            throw await FailAsync(res, method, pathOrUrl, authenticated);

        if (res.StatusCode == HttpStatusCode.NoContent || res.Content is null)
            return default;

        var text = await res.Content.ReadAsStringAsync();
        if (string.IsNullOrEmpty(text))
            return default;

        try
        {
            return JsonSerializer.Deserialize<T>(text, Json);
        }
        catch (JsonException e)
        {
            var error = new OtomoError((int)res.StatusCode, "bad_response", e.Message, RequestIdOf(res));
            LogFailure(method, pathOrUrl, error);
            throw error;
        }
    }

    public async Task SendAsync(
        HttpMethod method,
        string pathOrUrl,
        object? body = null,
        bool authenticated = true,
        TimeSpan? timeout = null,
        CancellationToken ct = default)
    {
        using var res = await SendCoreAsync(method, pathOrUrl, body, authenticated, timeout, ct);

        if (!res.IsSuccessStatusCode)
            throw await FailAsync(res, method, pathOrUrl, authenticated);

        if (res.Content is not null)
            await res.Content.ReadAsStringAsync();
    }

    /// Parse the shared error envelope {"error":{"code","message","request_id"}}. Public so
    /// patch downloads can reuse it (guide §3.2).
    public static async Task<OtomoError> ReadErrorAsync(HttpResponseMessage res)
    {
        var status = (int)res.StatusCode;
        var requestId = RequestIdOf(res);

        string? text = null;
        if (res.Content is not null)
            text = await res.Content.ReadAsStringAsync();

        if (!string.IsNullOrWhiteSpace(text))
        {
            try
            {
                using var doc = JsonDocument.Parse(text);
                if (doc.RootElement.ValueKind == JsonValueKind.Object &&
                    doc.RootElement.TryGetProperty("error", out var error) &&
                    error.ValueKind == JsonValueKind.Object)
                {
                    var code = StringOf(error, "code");
                    var message = StringOf(error, "message");
                    var bodyRequestId = StringOf(error, "request_id");

                    if (code is not null || message is not null)
                    {
                        return new OtomoError(
                            status,
                            code ?? "unknown",
                            message ?? res.ReasonPhrase ?? "",
                            string.IsNullOrEmpty(bodyRequestId) ? requestId : bodyRequestId!);
                    }
                }
            }
            catch (JsonException)
            {
                // Not the error envelope; fall through to the generic error.
            }
        }

        return new OtomoError(status, "unknown", res.ReasonPhrase ?? "", requestId);
    }

    /// First X-Request-Id header value, or "". The live edge sends the header twice.
    public static string RequestIdOf(HttpResponseMessage res)
    {
        if (res.Headers.TryGetValues("X-Request-Id", out var values))
        {
            foreach (var value in values)
                return value;
        }

        return "";
    }

    private async Task<HttpResponseMessage> SendCoreAsync(
        HttpMethod method,
        string pathOrUrl,
        object? body,
        bool authenticated,
        TimeSpan? timeout,
        CancellationToken ct)
    {
        var request = BuildRequest(method, pathOrUrl, body, authenticated);
        HttpResponseMessage response;
        try
        {
            response = await SendOnceAsync(request, timeout, ct);
        }
        finally
        {
            request.Dispose();
        }

        if (response.StatusCode == HttpStatusCode.Unauthorized && authenticated)
        {
            bool refreshed;
            try
            {
                refreshed = await RefreshAccessToken(ct);
            }
            catch
            {
                response.Dispose();
                throw;
            }

            if (refreshed)
            {
                response.Dispose();
                var retry = BuildRequest(method, pathOrUrl, body, authenticated);
                try
                {
                    response = await SendOnceAsync(retry, timeout, ct);
                }
                finally
                {
                    retry.Dispose();
                }
            }
        }

        if ((int)response.StatusCode == 409)
        {
            // The body is already buffered (SendOnceAsync uses ResponseContentRead), so
            // peeking it here is cheap and leaves it intact for the caller's own FailAsync
            // if this isn't the one 409 code that means "re-patch and retry".
            var peek = await ReadErrorAsync(response);
            if (peek.Code == "release_outdated")
            {
                bool releaseRefreshed;
                try
                {
                    releaseRefreshed = await RefreshRelease(ct);
                }
                catch
                {
                    response.Dispose();
                    throw;
                }

                if (releaseRefreshed)
                {
                    response.Dispose();
                    var retry = BuildRequest(method, pathOrUrl, body, authenticated);
                    try
                    {
                        response = await SendOnceAsync(retry, timeout, ct);
                    }
                    finally
                    {
                        retry.Dispose();
                    }
                }
            }
        }

        return response;
    }

    private async Task<OtomoError> FailAsync(HttpResponseMessage res, HttpMethod method, string pathOrUrl, bool authenticated)
    {
        var error = await ReadErrorAsync(res);
        LogFailure(method, pathOrUrl, error);

        if (authenticated && error.Status == (int)HttpStatusCode.Unauthorized)
            AuthenticationLost?.Invoke(error);

        return error;
    }

    private async Task<HttpResponseMessage> SendOnceAsync(HttpRequestMessage request, TimeSpan? timeout, CancellationToken ct)
    {
        using var linked = CancellationTokenSource.CreateLinkedTokenSource(ct);
        linked.CancelAfter(timeout ?? _config.RequestTimeout);

        try
        {
            return await _http.SendAsync(request, HttpCompletionOption.ResponseContentRead, linked.Token);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            throw new OtomoError(0, "timeout", "request timed out", "");
        }
        catch (HttpRequestException e)
        {
            throw new OtomoError(0, "network", e.Message, "", e);
        }
    }

    private HttpRequestMessage BuildRequest(HttpMethod method, string pathOrUrl, object? body, bool authenticated)
    {
        var request = new HttpRequestMessage(method, ResolveUrl(pathOrUrl));
        request.Headers.TryAddWithoutValidation("Accept", "application/json");

        if (authenticated)
        {
            var token = GetAccessToken();
            if (token is not null)
                request.Headers.TryAddWithoutValidation("Authorization", "Bearer " + token);
        }

        foreach (var header in ExtraHeaders)
            request.Headers.TryAddWithoutValidation(header.Key, header.Value);

        if (body is not null)
        {
            var json = JsonSerializer.Serialize(body);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        return request;
    }

    private string ResolveUrl(string pathOrUrl)
    {
        if (pathOrUrl.StartsWith("http://", StringComparison.Ordinal) ||
            pathOrUrl.StartsWith("https://", StringComparison.Ordinal))
            return pathOrUrl;

        return _config.BaseUrl + pathOrUrl;
    }

    private void LogFailure(HttpMethod method, string pathOrUrl, OtomoError error)
    {
        _config.Log?.Invoke(
            $"{method.Method} {PathOnly(pathOrUrl)} -> {error.Status} {error.Code} request_id={error.RequestId}");
    }

    private static string PathOnly(string pathOrUrl)
    {
        var query = pathOrUrl.IndexOf('?');
        if (query >= 0)
            pathOrUrl = pathOrUrl.Substring(0, query);

        if (Uri.TryCreate(pathOrUrl, UriKind.Absolute, out var uri) &&
            (uri.Scheme == "http" || uri.Scheme == "https"))
            return uri.AbsolutePath;

        return pathOrUrl;
    }

    private static string? StringOf(JsonElement element, string name) =>
        element.TryGetProperty(name, out var value) && value.ValueKind == JsonValueKind.String
            ? value.GetString()
            : null;
}
