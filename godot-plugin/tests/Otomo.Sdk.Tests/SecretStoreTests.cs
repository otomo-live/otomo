#nullable enable
using System;
using System.IO;
using OtomoSdk.Auth;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class SecretStoreTests
{
    private sealed class TempDir : IDisposable
    {
        public string Path { get; }

        public TempDir()
        {
            Path = System.IO.Path.Combine(System.IO.Path.GetTempPath(), "otomo-secret-" + Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(Path);
        }

        public void Dispose()
        {
            try
            {
                Directory.Delete(Path, recursive: true);
            }
            catch (DirectoryNotFoundException)
            {
                // Already gone.
            }
        }
    }

    [Fact]
    public void DeviceId_Is43UrlSafeChars_AndStableAcrossInstances()
    {
        using var dir = new TempDir();
        var first = new FileSecretStore(dir.Path).LoadOrCreateDeviceId();

        Assert.Equal(43, first.Length);
        Assert.Matches("^[A-Za-z0-9_-]{22,128}$", first);

        var second = new FileSecretStore(dir.Path).LoadOrCreateDeviceId();
        Assert.Equal(first, second);
    }

    [Fact]
    public void DeviceId_TrimsExistingFile()
    {
        using var dir = new TempDir();
        File.WriteAllText(System.IO.Path.Combine(dir.Path, "device_id"), "  device-from-disk  \n");

        var id = new FileSecretStore(dir.Path).LoadOrCreateDeviceId();

        Assert.Equal("device-from-disk", id);
    }

    [Fact]
    public void RefreshToken_SavesLoadsAndClears()
    {
        using var dir = new TempDir();
        var store = new FileSecretStore(dir.Path);

        Assert.Null(store.LoadRefreshToken());

        store.SaveRefreshToken("refresh-abc");
        Assert.Equal("refresh-abc", store.LoadRefreshToken());
        Assert.Equal("refresh-abc", new FileSecretStore(dir.Path).LoadRefreshToken());

        store.ClearRefreshToken();
        Assert.Null(store.LoadRefreshToken());

        // Clearing again is a no-op.
        store.ClearRefreshToken();
    }

    [Fact]
    public void BlankRefreshToken_LoadsAsNull()
    {
        using var dir = new TempDir();
        File.WriteAllText(System.IO.Path.Combine(dir.Path, "refresh_token"), "   ");

        Assert.Null(new FileSecretStore(dir.Path).LoadRefreshToken());
    }

    [Fact]
    public void Writes_LeaveNoTempFile()
    {
        using var dir = new TempDir();
        var store = new FileSecretStore(dir.Path);

        store.LoadOrCreateDeviceId();
        store.SaveRefreshToken("refresh-abc");

        Assert.False(File.Exists(System.IO.Path.Combine(dir.Path, "device_id.tmp")));
        Assert.False(File.Exists(System.IO.Path.Combine(dir.Path, "refresh_token.tmp")));
    }

    [Fact]
    public void Secrets_AreOwnerOnlyOnUnix_EvenOverALeftoverTempFile()
    {
        if (OperatingSystem.IsWindows()) return;
        using var dir = new TempDir();
        var leftover = System.IO.Path.Combine(dir.Path, "refresh_token.tmp");
        File.WriteAllText(leftover, "stale");
        File.SetUnixFileMode(leftover, (UnixFileMode)0b110_110_110);

        var store = new FileSecretStore(dir.Path);
        store.SaveRefreshToken("r1");
        store.LoadOrCreateDeviceId();

        var ownerOnly = UnixFileMode.UserRead | UnixFileMode.UserWrite;
        Assert.Equal(ownerOnly, File.GetUnixFileMode(System.IO.Path.Combine(dir.Path, "refresh_token")));
        Assert.Equal(ownerOnly, File.GetUnixFileMode(System.IO.Path.Combine(dir.Path, "device_id")));
        Assert.Equal("r1", store.LoadRefreshToken());
    }
}
