using System;
using Godot;
using OtomoSdk.Patch;

namespace OtomoSdk.Diagnostics;

/// Run OtomoSmokeTest.tscn (F6) to check that this build reaches Otomo: it runs the whole
/// start-up sequence and prints each step. Headless, for CI or a quick check:
///   godot --headless --path <project> res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn -- --otomo-smoke-quit
/// Exit code 0 = patched and logged in, 1 = anything else.
public partial class OtomoSmokeTest : Node
{
    public override async void _Ready()
    {
        var otomo = Otomo.Instance;
        if (otomo is null)
        {
            GD.PushError("otomo smoke: the Otomo autoload is missing; enable the Otomo SDK plugin.");
            Finish(false);
            return;
        }

        otomo.PatchStateChanged += s => GD.Print($"otomo smoke: patch state {s}");
        try
        {
            var r = await otomo.StartAsync();
            var manifest = otomo.Client.Patch.Current;
            GD.Print($"otomo smoke: patch={r.Patch} release={manifest?.ReleaseId.ToString() ?? "none"} " +
                     $"config_docs=[{string.Join(", ", otomo.RemoteConfig.Names)}] " +
                     $"packs=[{string.Join(", ", manifest?.Packs.ConvertAll(p => p.Name) ?? new())}]");
            GD.Print($"otomo smoke: logged_in={r.LoggedIn} session={r.Session} profile={r.Profile?.ToString() ?? "-"}");
            if (r.Error is not null) GD.PushError($"otomo smoke: {r.Error}");
            Finish(r.LoggedIn && r.Patch != PatchResult.ClientTooOld);
        }
        catch (Exception e)
        {
            GD.PushError($"otomo smoke: unexpected {e}");
            Finish(false);
        }
    }

    void Finish(bool ok)
    {
        GD.Print(ok ? "otomo smoke: OK" : "otomo smoke: FAILED");
        if (Array.IndexOf(OS.GetCmdlineUserArgs(), "--otomo-smoke-quit") >= 0)
            GetTree().Quit(ok ? 0 : 1);
    }
}
