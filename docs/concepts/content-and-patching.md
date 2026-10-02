# Content and patching

Patching is how your team changes a live game without shipping a new build: a balance
tweak, a seasonal banner, a new set of textures. This page explains what can be changed that
way and what can't, the words the admin website uses (namespace, version, pack, release,
channel), and exactly what the game does when it updates.

## The one rule to remember

!!! warning "Content can be patched. Code can't."
    Otomo can deliver **data**: numbers, text, colours, on/off switches, and Godot
    resources such as textures, sounds and scenes. It can't deliver new **C# code**.
    A change to how something *behaves* still needs a new build of the game.

Why? When you build a Godot C# game, all your C# scripts are compiled into one program file
that ships with the game. Godot can load extra *resources* while the game runs, but it can't
swap that program file for a new one. So:

| Change | Patchable? |
|---|---|
| The fireball does 20 damage instead of 15 | **Yes**: a number in a settings document |
| A "Double XP" banner this weekend | **Yes**: text and a switch in a settings document |
| A new texture for the winter event | **Yes**: a texture in a content pack |
| The fireball now bounces off walls | **No**: new behaviour, which is C# code |
| A new enemy type with its own AI | **No**: new code |

A good habit follows from this: **put tunable numbers in settings documents from the
start**, instead of hard-coding them. Then designers can balance the live game themselves.

## The building blocks

The admin website organises content with five ideas. It's worth reading this section
slowly, because every guide uses these words.

### Namespace: a named settings document

A **namespace** is one JSON settings document with a name, such as `balance.player` or
`ui.motd` (the message of the day). For example, `balance.player` might contain:

```json
{
  "max_hp": 100,
  "move_speed": 5.5,
  "spells": { "fireball": { "damage": 20, "cooldown": 1.0 } }
}
```

Names are lowercase words joined by dots: `balance.player`, `events.winter`. The dots are
only for tidiness; they don't nest one namespace inside another.

Each namespace has an **audience**, chosen when it's created and never changed:

- **Client**: sent to players' games. Use this for anything the game reads.
- **Server**: read only by otomo's own services and never sent to players. For example,
  `session.rules` sets the maximum party size. Players can't read these, so they're the
  place for anything players shouldn't see.

A namespace can also have a **schema**: a description of what its JSON must look like
("`max_hp` must be a whole number between 1 and 1000"). The admin website uses it to show a
form and to catch mistakes. A new namespace starts with an empty schema that accepts any
JSON.

### Version: a frozen snapshot of a namespace

While you edit a namespace, your changes are a **draft**. Nobody sees a draft except the
team. When the document is right, you **create a version**: a numbered, frozen copy (v1,
v2, v3…) that can never change again. Freezing matters because a release (below) must
always mean exactly the same content, even months later.

### Pack: a bundle of Godot resources

A **pack** is a Godot `.pck` file: a bundle of resources such as textures, sounds, scenes
and text files, with their `res://` paths inside. You make one with Godot (see
[Publish your first content update](../guide/first-patch.md)) and upload it to the admin
website. Pack names are lowercase letters, digits and underscores, such as `winter_event`.

!!! warning "A pack can add files, not replace them"
    Otomo mounts packs so that they **can't overwrite files that shipped with the game**.
    If your build contains `res://ui/banner.png`, a pack containing the same path is
    ignored for that file. Put patchable content at paths the build doesn't have, for
    example `res://patch/winter/banner.png`, and have your code look there first.

### Release: what players get

A **release** is the complete answer to "what content should players have right now?". It
lists:

- one version of each client namespace you include;
- the packs to include;
- a **minimum client version**: the oldest game build allowed to use this content.

Every release gets a number, its `release_id`. Like versions, a release never changes once
published.

### Channel: which audience gets a release

A **channel** is a named stream of releases. There are three:

| Channel | Who receives it |
|---|---|
| `dev` | Developers testing new content |
| `staging` | A final check before players |
| `live` | **Every player** |

Each channel has one **head**: the release it's currently serving. Publishing a release to
`live` makes it the head of `live`, and every player gets it the next time their game
checks.

**Rolling back** makes an older release the head again. That's how you undo a mistake:
nothing is deleted, the channel just points at the earlier release. **Promoting** copies the
head of the previous channel in line: `dev` to `staging`, or `staging` to `live` once it's
been checked.

## What the game does when it patches

When your game calls `StartAsync` (see the [SDK reference](../reference/sdk.md)), the SDK
runs these steps before anything else. The state names in capitals are what the SDK
reports as it goes:

1. **CHECKING.** It asks Patch for the head of its channel's **manifest**: a small JSON list
   of every file in the current release, each with its size and **SHA-256 fingerprint**.
   A fingerprint is a 64-character code computed from a file's exact bytes; change one byte
   and the fingerprint changes completely.

    The SDK remembers a tag for the last manifest it got (the `ETag`). If nothing has
    changed, Patch answers `304 Not Modified` with no body, and the check costs almost
    nothing.

2. **Version check.** If the game build is older than the release's minimum client
   version, the SDK stops with `ClientTooOld`. Your game should tell the player to update.

3. **DOWNLOADING.** For every file it doesn't already have, it downloads the file. If a
   download is interrupted, the next attempt continues where it stopped instead of starting
   over.

4. **VERIFYING.** It computes each downloaded file's fingerprint and compares it with the
   manifest. A file that doesn't match (damaged on the way, or tampered with) is thrown
   away and fetched again.

5. **MOUNTING.** It loads the settings documents into **RemoteConfig** (below), and adds
   each pack to Godot's resource system so `res://` paths inside it work, just like files
   from the build.

6. **READY.** It saves the manifest as the "last good" release, and deletes old files the
   release no longer lists.

### When the server can't be reached

If Patch doesn't answer (no internet, or the server is down), the SDK uses the last good
release saved on the player's computer and reports `OfflineReady`. The game still starts,
with the content it had last time. On the very first run with no internet, there's nothing
saved, so the game runs with no settings documents and no packs, and your code's
**fallback values** (see below) are used.

### When changes reach players

The game checks for updates **when it starts**. A player who's already playing gets the new
release the next time they launch the game.

## Reading settings in your game

After patching, every client namespace in the release is available through
`Otomo.Instance.RemoteConfig`. You ask for a value by namespace and key, and **always give a
fallback**, the value to use if the document or key is missing:

```csharp
var config = Otomo.Instance.RemoteConfig;

// "balance.player" is the namespace; "max_hp" is the key inside it.
// 100 is used if the key is missing (for example, offline on first run).
int maxHp = config.GetInt("balance.player", "max_hp", 100);

// Dots in the key reach into nested objects: spells → fireball → damage.
float damage = config.GetFloat("balance.player", "spells.fireball.damage", 15f);

bool showBanner = config.GetBool("ui.motd", "show", false);
string text = config.GetString("ui.motd", "text", "");
```

The fallback also protects you from typos in the admin website: if a designer writes
`"max_hp": "lots"` (text instead of a number), `GetInt` returns the fallback instead of
crashing the game.

## Next

Continue with [Parties and matches](parties-and-matches.md), or, if you want to try
patching right away, go to [Publish your first content update](../guide/first-patch.md).
