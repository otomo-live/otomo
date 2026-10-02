#nullable enable
using System;
using System.Globalization;

namespace OtomoSdk;

/// Tiny invariant formatting helpers for launcher UI ("Download 12.3 MB?", "3 min 5 s").
public static class OtomoFormat
{
    private static readonly string[] Units = { "B", "KB", "MB", "GB", "TB", "PB" };

    /// 1024-based byte size: "0 B", "512 B", "1.5 KB", "12.3 MB", "1.02 GB".
    public static string Bytes(long bytes)
    {
        if (bytes <= 0)
            return "0 B";

        double value = bytes;
        var unit = 0;
        while (value >= 1024 && unit < Units.Length - 1)
        {
            value /= 1024;
            unit++;
        }

        // Below 100 we keep a fractional part (at most two decimals); above it a whole number.
        var format = unit == 0
            ? "0"
            : value < 100 ? "0.##" : "0";

        return value.ToString(format, CultureInfo.InvariantCulture) + " " + Units[unit];
    }

    /// Duration in seconds: null → "--", 12 s, 3 min 5 s, 1 h 2 min.
    public static string Duration(double? seconds)
    {
        if (seconds is null)
            return "--";

        var total = (long)Math.Round(seconds.Value, MidpointRounding.AwayFromZero);
        if (total < 0)
            total = 0;

        if (total < 60)
            return total.ToString(CultureInfo.InvariantCulture) + " s";

        var minutes = total / 60;
        var rest = total % 60;

        if (minutes < 60)
            return minutes.ToString(CultureInfo.InvariantCulture)
                + " min "
                + rest.ToString(CultureInfo.InvariantCulture)
                + " s";

        var hours = minutes / 60;
        var remainingMinutes = minutes % 60;
        return hours.ToString(CultureInfo.InvariantCulture)
            + " h "
            + remainingMinutes.ToString(CultureInfo.InvariantCulture)
            + " min";
    }
}
