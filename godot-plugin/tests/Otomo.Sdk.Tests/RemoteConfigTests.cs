#nullable enable
using System.Collections.Generic;
using OtomoSdk.Patch;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class RemoteConfigTests
{
    [Fact]
    public void TryGet_WalksDottedPath()
    {
        var config = new RemoteConfig();
        config.Load("game", "{\"enemies\":{\"slime\":{\"hp\":12}}}");

        Assert.True(config.TryGet("game", "enemies.slime.hp", out var value));
        Assert.Equal(12, value.GetInt32());
        Assert.False(config.TryGet("game", "enemies.slime.mana", out _));
        Assert.False(config.TryGet("missing", "enemies.slime.hp", out _));
    }

    [Fact]
    public void TypedGetters_ReturnFallbackOnWrongKindOrMissing()
    {
        var config = new RemoteConfig();
        config.Load("game", "{\"n\":3.5,\"i\":7,\"b\":true,\"s\":\"hi\"}");

        Assert.Equal(3.5f, config.GetFloat("game", "n", -1f));
        Assert.Equal(7, config.GetInt("game", "i", -1));
        Assert.True(config.GetBool("game", "b", false));
        Assert.Equal("hi", config.GetString("game", "s", "fallback"));

        Assert.Equal(-1f, config.GetFloat("game", "s", -1f));
        Assert.Equal(-1, config.GetInt("game", "n", -1));
        Assert.False(config.GetBool("game", "i", false));
        Assert.Equal("fallback", config.GetString("game", "n", "fallback"));

        Assert.Equal(-1, config.GetInt("missing", "i", -1));
        Assert.Equal("fallback", config.GetString("missing", "i", "fallback"));
    }

    [Fact]
    public void ReplaceAll_RaisesChangedOnce()
    {
        var config = new RemoteConfig();
        var raised = 0;
        config.Changed += () => raised++;

        config.ReplaceAll(new Dictionary<string, string>
        {
            ["a"] = "{\"x\":1}",
            ["b"] = "{\"y\":2}",
        });

        Assert.Equal(1, raised);
        Assert.Equal(2, config.Names.Count);
        Assert.True(config.TryGet("a", "x", out var value));
        Assert.Equal(1, value.GetInt32());
    }

    [Fact]
    public void Clear_RemovesAllDocuments()
    {
        var config = new RemoteConfig();
        config.Load("a", "{\"x\":1}");
        config.Clear();

        Assert.Empty(config.Names);
        Assert.False(config.TryGet("a", "x", out _));
    }

    [Fact]
    public void Load_InvalidJson_IsLoggedAndIgnored()
    {
        var logs = new List<string>();
        var config = new RemoteConfig(logs.Add);

        config.Load("a", "{not json");

        Assert.False(config.TryGet("a", "x", out _));
        Assert.Single(logs);
    }
}
