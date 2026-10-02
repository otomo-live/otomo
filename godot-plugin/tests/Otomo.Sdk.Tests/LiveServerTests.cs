#nullable enable
using System;
using System.IO;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk;
using OtomoSdk.Patch;
using Xunit;

namespace Otomo.Sdk.Tests;

/// Runs the real start-up sequence against a live gateway. Opt-in: set
/// OTOMO_LIVE_BASE_URL (e.g. https://play.example.com); without it the test returns
/// immediately. Uses a throwaway data dir, so it creates a fresh anonymous account each run.
public sealed class LiveServerTests
{
    sealed class NoMount : IPackMounter
    {
        public bool Mount(string osPath, ManifestPack pack) => true;
    }

    [Fact]
    public async Task Live_PatchLoginSessionRefreshLogout()
    {
        var baseUrl = Environment.GetEnvironmentVariable("OTOMO_LIVE_BASE_URL");
        if (string.IsNullOrWhiteSpace(baseUrl)) return;

        var dir = Path.Combine(Path.GetTempPath(), "otomo-live-" + Guid.NewGuid().ToString("N"));
        using var http = new HttpClient { Timeout = Timeout.InfiniteTimeSpan };
        var log = new System.Collections.Generic.List<string>();
        var config = new OtomoConfig(baseUrl, dir) { ClientVersion = "1.0.0", Log = log.Add };
        try
        {
            var sdk = new OtomoClient(http, config, new NoMount());
            var first = await sdk.StartAsync();

            Assert.Null(first.Error);
            Assert.Equal(PatchResult.Ready, first.Patch);
            Assert.True(first.LoggedIn);
            Assert.NotEqual(SessionStatus.Failed, first.Session);   // 501 today, a profile later
            Assert.True(File.Exists(Path.Combine(dir, "patch", "manifest.json")));
            var refreshAfterLogin = File.ReadAllText(Path.Combine(dir, "refresh_token"));

            // A second client on the same data dir: manifest is a 304, login goes through
            // /auth/refresh with the saved token, which rotates it.
            var again = new OtomoClient(http, config, new NoMount());
            var second = await again.StartAsync();
            Assert.Equal(PatchResult.Ready, second.Patch);
            Assert.True(second.LoggedIn);
            Assert.NotEqual(refreshAfterLogin, File.ReadAllText(Path.Combine(dir, "refresh_token")));

            await again.Auth.LogoutAsync();
            Assert.False(File.Exists(Path.Combine(dir, "refresh_token")));
            Assert.True(File.Exists(Path.Combine(dir, "device_id")));

            var deviceId = File.ReadAllText(Path.Combine(dir, "device_id"));
            Assert.DoesNotContain(log, l => l.Contains(deviceId, StringComparison.Ordinal));
        }
        finally
        {
            try { Directory.Delete(dir, recursive: true); } catch (IOException) { }
        }
    }
}
