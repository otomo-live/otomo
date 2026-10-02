#nullable enable
using System;
using System.IO;
using System.Security.Cryptography;

namespace OtomoSdk.Auth;

/// <summary>
/// File-backed <see cref="ISecretStore"/>. Device ID and refresh token live in two small
/// files under the SDK data directory. Writes go through a ".tmp" sibling and an atomic
/// move so a crash never leaves a half-written secret. Never logs the values.
/// </summary>
public sealed class FileSecretStore : ISecretStore
{
    private readonly string _deviceIdPath;
    private readonly string _refreshTokenPath;

    public FileSecretStore(string dataDir)
    {
        if (string.IsNullOrWhiteSpace(dataDir)) throw new ArgumentException("data dir is required", nameof(dataDir));
        _deviceIdPath = Path.Combine(dataDir, "device_id");
        _refreshTokenPath = Path.Combine(dataDir, "refresh_token");
    }

    public string LoadOrCreateDeviceId()
    {
        if (File.Exists(_deviceIdPath))
        {
            var existing = File.ReadAllText(_deviceIdPath);
            if (!string.IsNullOrWhiteSpace(existing))
                return existing.Trim();
        }

        var deviceId = Base64Url(RandomNumberGenerator.GetBytes(32));
        WriteAtomic(_deviceIdPath, deviceId);
        return deviceId;
    }

    public string? LoadRefreshToken()
    {
        if (!File.Exists(_refreshTokenPath))
            return null;

        var token = File.ReadAllText(_refreshTokenPath);
        return string.IsNullOrWhiteSpace(token) ? null : token.Trim();
    }

    public void SaveRefreshToken(string token) => WriteAtomic(_refreshTokenPath, token);

    public void ClearRefreshToken()
    {
        try
        {
            File.Delete(_refreshTokenPath);
        }
        catch (FileNotFoundException)
        {
            // Already gone.
        }
        catch (DirectoryNotFoundException)
        {
            // Nothing to clear.
        }
    }

    private static string Base64Url(byte[] bytes) =>
        Convert.ToBase64String(bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_');

    private static void WriteAtomic(string path, string content)
    {
        var directory = Path.GetDirectoryName(path);
        if (!string.IsNullOrEmpty(directory))
            Directory.CreateDirectory(directory);

        // Create the temp file owner-only from the start (a chmod after writing would leave
        // the secret readable for a moment). Delete a leftover first: UnixCreateMode only
        // applies to a file that is created.
        var temp = path + ".tmp";
        File.Delete(temp);
        var options = new FileStreamOptions { Mode = FileMode.CreateNew, Access = FileAccess.Write };
        if (!OperatingSystem.IsWindows())
            options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
        using (var writer = new StreamWriter(temp, options))
            writer.Write(content);

        File.Move(temp, path, overwrite: true);
    }
}
