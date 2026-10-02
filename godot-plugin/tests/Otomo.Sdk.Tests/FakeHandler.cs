#nullable enable
using System;
using System.Collections.Generic;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;

namespace Otomo.Sdk.Tests;

/// Copy of one request seen by FakeHandler. Headers/body are copied because the caller
/// disposes the HttpRequestMessage once the response comes back.
public sealed class RecordedRequest
{
    public HttpMethod Method { get; }
    public Uri? RequestUri { get; }
    public string? Authorization { get; }
    public string? Body { get; }
    public IReadOnlyDictionary<string, string> Headers { get; }

    public RecordedRequest(
        HttpMethod method,
        Uri? requestUri,
        string? authorization,
        string? body,
        IReadOnlyDictionary<string, string> headers)
    {
        Method = method;
        RequestUri = requestUri;
        Authorization = authorization;
        Body = body;
        Headers = headers;
    }
}

/// Queue-driven fake transport. Each SendAsync records the request, optionally waits
/// <see cref="Delay"/> (for timeout tests), then runs the next responder. Throwing responders
/// simulate HttpRequestException.
public sealed class FakeHandler : HttpMessageHandler
{
    private readonly Queue<Func<HttpRequestMessage, HttpResponseMessage>> _responders = new();
    private readonly object _gate = new();

    public List<RecordedRequest> Requests { get; } = new();

    /// Optional delay before the responder runs; intentionally honors the cancellation token.
    public TimeSpan Delay { get; set; }

    /// Used when the queue is empty. Lets a test answer an unbounded number of requests
    /// (and branch on the request) instead of enqueueing one responder per request.
    public Func<HttpRequestMessage, HttpResponseMessage>? FallbackResponder { get; set; }

    public FakeHandler Respond(Func<HttpRequestMessage, HttpResponseMessage> responder)
    {
        _responders.Enqueue(responder);
        return this;
    }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var header in request.Headers)
            headers[header.Key] = string.Join(",", header.Value);

        string? body = null;
        if (request.Content is not null)
        {
            body = await request.Content.ReadAsStringAsync(cancellationToken);
            foreach (var header in request.Content.Headers)
                headers[header.Key] = string.Join(",", header.Value);
        }

        headers.TryGetValue("Authorization", out var authorization);
        lock (_gate)
            Requests.Add(new RecordedRequest(request.Method, request.RequestUri, authorization, body, headers));

        if (Delay > TimeSpan.Zero)
            await Task.Delay(Delay, cancellationToken);

        Func<HttpRequestMessage, HttpResponseMessage> responder;
        lock (_gate)
        {
            if (_responders.Count > 0)
                responder = _responders.Dequeue();
            else if (FallbackResponder is not null)
                responder = FallbackResponder;
            else
                throw new InvalidOperationException("FakeHandler: no responder queued");
        }

        return responder(request);
    }
}
