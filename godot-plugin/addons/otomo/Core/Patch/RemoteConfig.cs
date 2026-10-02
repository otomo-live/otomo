#nullable enable
using System;
using System.Collections.Generic;
using System.Text.Json;

namespace OtomoSdk.Patch;

/// Client-visible config documents (JSON objects) keyed by name. Values are cloned out of
/// the parsed document so the JsonDocument can be disposed immediately.
public sealed class RemoteConfig
{
    private readonly Action<string>? _log;
    private readonly Dictionary<string, JsonElement> _docs = new(StringComparer.Ordinal);

    public RemoteConfig(Action<string>? log = null) => _log = log;

    /// Raised once after <see cref="ReplaceAll"/> finishes applying a batch.
    public event Action? Changed;

    public IReadOnlyCollection<string> Names => _docs.Keys;

    public void Load(string name, string json)
    {
        try
        {
            using var doc = JsonDocument.Parse(json);
            _docs[name] = doc.RootElement.Clone();
        }
        catch (JsonException e)
        {
            _log?.Invoke($"remote config '{name}' is not valid JSON: {e.Message}");
        }
    }

    public void Clear() => _docs.Clear();

    /// Clear, load every document, then raise <see cref="Changed"/> exactly once.
    public void ReplaceAll(IDictionary<string, string> docs)
    {
        if (docs is null) throw new ArgumentNullException(nameof(docs));

        _docs.Clear();
        foreach (var pair in docs)
            Load(pair.Key, pair.Value);

        Changed?.Invoke();
    }

    /// Dotted lookup into nested objects ("enemies.slime.hp"). An empty key returns the doc root.
    public bool TryGet(string doc, string key, out JsonElement value)
    {
        value = default;
        if (!_docs.TryGetValue(doc, out var current))
            return false;

        if (string.IsNullOrEmpty(key))
        {
            value = current;
            return true;
        }

        foreach (var part in key.Split('.'))
        {
            if (current.ValueKind != JsonValueKind.Object)
                return false;
            if (!current.TryGetProperty(part, out current))
                return false;
        }

        value = current;
        return true;
    }

    public float GetFloat(string doc, string key, float fallback) =>
        TryGet(doc, key, out var value)
        && value.ValueKind == JsonValueKind.Number
        && value.TryGetSingle(out var result)
            ? result
            : fallback;

    public int GetInt(string doc, string key, int fallback) =>
        TryGet(doc, key, out var value)
        && value.ValueKind == JsonValueKind.Number
        && value.TryGetInt32(out var result)
            ? result
            : fallback;

    public bool GetBool(string doc, string key, bool fallback)
    {
        if (!TryGet(doc, key, out var value))
            return fallback;

        return value.ValueKind switch
        {
            JsonValueKind.True => true,
            JsonValueKind.False => false,
            _ => fallback,
        };
    }

    public string GetString(string doc, string key, string fallback) =>
        TryGet(doc, key, out var value) && value.ValueKind == JsonValueKind.String
            ? value.GetString() ?? fallback
            : fallback;
}
