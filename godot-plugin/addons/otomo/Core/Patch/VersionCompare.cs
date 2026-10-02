#nullable enable
using System.Globalization;

namespace OtomoSdk.Patch;

/// Compares the game's own version with a manifest's min_client_version. Parts are
/// compared numerically; a missing or non-numeric part counts as zero.
public static class VersionCompare
{
    public static bool IsOlder(string mine, string minimum)
    {
        if (string.IsNullOrWhiteSpace(minimum))
            return false;

        var a = Parse(mine);
        var b = Parse(minimum);

        for (var i = 0; i < 3; i++)
        {
            if (a[i] != b[i])
                return a[i] < b[i];
        }

        return false;
    }

    private static int[] Parse(string? version)
    {
        var parts = new int[3];
        if (string.IsNullOrWhiteSpace(version))
            return parts;

        var split = version!.Split('.');
        for (var i = 0; i < 3 && i < split.Length; i++)
        {
            var text = split[i].Trim();
            if (int.TryParse(text, NumberStyles.None, CultureInfo.InvariantCulture, out var value) && value >= 0)
                parts[i] = value;
            else
                parts[i] = 0;
        }

        return parts;
    }
}
