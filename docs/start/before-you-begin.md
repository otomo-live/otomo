# Before you begin

This page lists what to set up before following the guides: a server to run otomo on, and
a PC ready to build your game with the SDK.

!!! note "Take this page slowly"
    Nearly every problem people hit in the guides comes from a missing step here. It's
    worth ten careful minutes now rather than an hour of confusion later.

## For the server: a VM and a domain name

Otomo runs on a single Linux machine that you control. A **virtual machine** (VM) from any
cloud provider works ([Servers and containers](../concepts/servers-and-containers.md)
explains what a VM is). You need:

| What | Why | Minimum |
|---|---|---|
| **A Linux VM** running Ubuntu 24.04 | Runs every part of otomo, in containers | 2 CPU cores, 4 GB memory, 40 GB disk |
| **SSH access** to it, with `sudo` | You install and manage otomo from the terminal | Your cloud provider sets this up when you create the VM |
| **A domain name** with an **A record** pointing at the VM's IP | Players' games reach otomo over HTTPS, which needs a name, not just an IP | Any domain you control, such as `play.example.com` |
| **Firewall openings** at your cloud provider | So players can reach otomo | TCP 22, 80 and 443; UDP 27000 |

You don't need to install anything on the VM yet. [Deploy otomo on a VM](deploy-on-a-vm.md)
does it step by step.

!!! tip "Trying otomo out first?"
    The smallest VM size that meets the minimum is enough for a team testing with a few
    players. You can resize the VM later without reinstalling otomo.

## For your game: Godot with C#

Otomo's SDK (the part that goes into your game) is written in **C#**, so you need the
**.NET edition** of Godot, not the standard one.

1. Install **Godot 4.7.2 .NET**. The download page has two versions of each release; pick
   the one labelled **.NET**.
2. Install the **.NET SDK** (version 8 or newer) from Microsoft's website. Godot needs it
   to build C# code.
3. Check both work: open your project in Godot and click **Build** (the hammer icon, top
   right). It should finish with no errors in the **MSBuild** panel at the bottom.

!!! tip "Two Godot programs in the download"
    The Windows download contains `Godot_v4.7.2_mono_win64.exe` and
    `Godot_v4.7.2_mono_win64_console.exe`, or similar names. They're the same editor. The
    `console` one also opens a black text window showing everything the game prints. That
    window is handy for the command-line steps in some guides.

### Is my project a C# project?

A Godot project can use C# only if it has a `.csproj` file in its folder (for example
`MyGame.csproj`). If yours doesn't:

1. In Godot, right-click any folder in the **FileSystem** panel → **New Script…**.
2. Choose **C#** as the language and create it.
3. Godot creates the `.csproj` for you. You can delete that script afterwards.

## For your team: SSH keys and admin accounts

The admin website is where your team publishes content. It's private: it isn't on the
public internet, so each person's computer reaches it through a secure connection to the
VM, called an **SSH tunnel**. Everyone who uses the admin website needs:

1. **A login on the VM.** Each person makes an **SSH key**
   ([Command-line basics](../concepts/command-line-basics.md) shows how) and sends you the
   *public* half; you add it to the VM.
2. **An admin website account.** Once otomo is running, you create it from the **Users**
   page and send an invite link. [Use the admin website](../guide/admin-website.md) walks
   through it.

## On Windows, for anything with commands: WSL

The guides ask you to type commands meant for **Linux**, the operating system otomo runs
on. Windows can run them through **WSL** (Windows Subsystem for Linux), a real Linux
inside Windows that Microsoft provides.

1. Open **PowerShell** as administrator (Start menu → type `PowerShell` → right-click →
   **Run as administrator**).
2. Type this and press **Enter**:

    ```powershell
    wsl --install -d Ubuntu
    ```

    This downloads Ubuntu, a popular version of Linux, and sets it up.

3. Restart your computer when asked. After the restart, an **Ubuntu** window opens and
   asks for a new user name and password. These belong to your Linux inside Windows only,
   and don't have to match your Windows login.
4. From now on, "open WSL" means: Start menu → type `Ubuntu` → open it.

!!! warning "Use SSH from WSL, not from Windows"
    On some Windows machines, the `ssh` command built into Windows fails with
    `Host key verification failed` where the same command works from WSL. When a guide
    uses `ssh` or `scp`, run it in WSL.

## Checklist

- [ ] I have a Linux VM I can log in to with SSH, and `sudo` works.
- [ ] My domain name's A record points at the VM's IP (`nslookup play.example.com` shows it).
- [ ] My cloud provider's firewall allows TCP 22, 80, 443 and UDP 27000.
- [ ] Godot 4.7.2 **.NET** opens my project, and **Build** succeeds.
- [ ] My project has a `.csproj` file.
- [ ] *(On Windows)* WSL with Ubuntu is installed and opens.

## Next

[Deploy otomo on a VM](deploy-on-a-vm.md). If you're new to online games, read the Concepts
pages first, starting with [How games go online](../concepts/how-games-go-online.md).
