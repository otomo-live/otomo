#if TOOLS
using Godot;

namespace OtomoSdk;

/// Enabling the plugin (Project → Project Settings → Plugins) registers the Otomo autoload
/// and the otomo/config/* project settings. Disabling removes the autoload; the settings
/// are left so their values aren't lost.
[Tool]
public partial class OtomoPlugin : EditorPlugin
{
    const string AutoloadName = "Otomo";

    public override void _EnablePlugin()
    {
        AddSetting(Otomo.SettingBaseUrl, "http://localhost:8080", Variant.Type.String);
        AddSetting(Otomo.SettingClientVersion, "", Variant.Type.String);
        AddSetting(Otomo.SettingChannel, "live", Variant.Type.String);
        AddSetting(Otomo.SettingVerboseLog, true, Variant.Type.Bool);
        AddSetting(Otomo.SettingConfirmDownloadOverBytes, -1L, Variant.Type.Int);
        ProjectSettings.Save();
        AddAutoloadSingleton(AutoloadName, "res://addons/otomo/Otomo.cs");
    }

    public override void _DisablePlugin()
    {
        RemoveAutoloadSingleton(AutoloadName);
    }

    static void AddSetting(string name, Variant initial, Variant.Type type)
    {
        if (!ProjectSettings.HasSetting(name)) ProjectSettings.SetSetting(name, initial);
        ProjectSettings.SetInitialValue(name, initial);
        ProjectSettings.AddPropertyInfo(new Godot.Collections.Dictionary
        {
            { "name", name },
            { "type", (int)type },
        });
    }
}
#endif
