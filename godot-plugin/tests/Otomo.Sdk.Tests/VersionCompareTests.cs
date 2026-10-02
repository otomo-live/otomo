#nullable enable
using OtomoSdk.Patch;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class VersionCompareTests
{
    [Theory]
    [InlineData("1.4.0", "1.10.2", true)]
    [InlineData("1.10.2", "1.4.0", false)]
    [InlineData("1.4.0", "1.4.0", false)]
    [InlineData("1.4", "1.4.0", false)]
    [InlineData("1.4.0", "1.4", false)]
    [InlineData("1.4", "1.4.1", true)]
    [InlineData("2.0", "1.9.9", false)]
    [InlineData("1.9.9", "2.0", true)]
    [InlineData("garbage", "1.0.0", true)]
    [InlineData("1.0.0", "garbage", false)]
    [InlineData("", "0.0.0", false)]
    [InlineData("1.4.0.5", "1.4.0", false)]
    public void IsOlder_ComparesPartsNumerically(string mine, string minimum, bool expected)
    {
        Assert.Equal(expected, VersionCompare.IsOlder(mine, minimum));
    }
}
