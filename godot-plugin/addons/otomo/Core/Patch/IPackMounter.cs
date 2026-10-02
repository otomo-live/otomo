#nullable enable

namespace OtomoSdk.Patch;

/// Godot layer plugs in ProjectSettings.LoadResourcePack here. Core stays Godot-free.
public interface IPackMounter
{
    bool Mount(string osPath, ManifestPack pack);
}
