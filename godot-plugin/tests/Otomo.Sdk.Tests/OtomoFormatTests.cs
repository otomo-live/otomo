#nullable enable
using OtomoSdk;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class OtomoFormatTests
{
    [Theory]
    [InlineData(0L, "0 B")]
    [InlineData(512L, "512 B")]
    [InlineData(1536L, "1.5 KB")]
    [InlineData(12_897_484L, "12.3 MB")]
    [InlineData(1_095_216_660L, "1.02 GB")]
    public void Bytes_FormatsInvariantBinaryUnits(long bytes, string expected) =>
        Assert.Equal(expected, OtomoFormat.Bytes(bytes));

    [Theory]
    [InlineData(12.0, "12 s")]
    [InlineData(185.0, "3 min 5 s")]
    public void Duration_FormatsSeconds(double seconds, string expected) =>
        Assert.Equal(expected, OtomoFormat.Duration(seconds));

    [Fact]
    public void Duration_Null_IsPlaceholder() =>
        Assert.Equal("--", OtomoFormat.Duration(null));
}
