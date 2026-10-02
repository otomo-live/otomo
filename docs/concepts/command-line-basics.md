# Command-line basics

Some guides ask you to type commands instead of clicking buttons. This page teaches
everything those guides assume: what a terminal is, how to read a command, how to move
between folders, and how to connect to your VM and copy files to it. It's written
for someone who has never used one.

## What a terminal is

A **terminal** is a window where you control the computer by typing instead of clicking.
You type a **command**, press **Enter**, and the computer prints its answer below. Then it
waits for the next command.

The program inside the terminal that reads your commands is called a **shell**. Different
systems have different shells, and they don't all understand the same commands:

| Shell | Where | How to open it |
|---|---|---|
| **PowerShell** | Windows | Start menu → type `PowerShell` |
| **Command Prompt** (`cmd`) | Windows, older | Start menu → type `cmd` |
| **bash** | Linux, including WSL and your VM | On Windows: open **Ubuntu** (WSL, see [Before you begin](../start/before-you-begin.md)) |

Each guide says which shell a command is for. When a code block starts with `sh` or shows
paths like `/mnt/c/…` or `~/otomo`, it's for **bash** (WSL or the server). When it shows
paths like `C:\…`, it's for Windows.

!!! tip "Copy, don't retype"
    Every code block on this wiki has a copy button in its top-right corner. Copy the
    command and paste it into the terminal:

    - **Windows Terminal / PowerShell:** right-click or <kbd>Ctrl</kbd>+<kbd>V</kbd>.
    - **Ubuntu (WSL) window:** right-click, or <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>V</kbd>. A plain <kbd>Ctrl</kbd>+<kbd>V</kbd>
      doesn't paste in some Linux terminals.

    Most "command not found" and "no such file" errors are typing mistakes.

## Reading a command

A command is a program name followed by extra words, separated by spaces:

```sh
docker compose logs --tail 20 gs-1
```

- `docker` is the **program** to run.
- `compose logs` are **subcommands**: which of the program's jobs to do (show the logs).
- `--tail 20` is an **option** (also called a **flag**): a setting that changes how the job
  is done. It starts with `-` or `--`. Here, "only the last 20 lines".
- `gs-1` is an **argument**: what to do it *to* (the container named `gs-1`).

Options with one dash are usually single letters (`-i`, `-d`). Several can be combined:
`-la` means `-l -a`.

In documentation, a word in angle brackets such as `<your-name>` is a **placeholder**:
replace the whole thing, brackets included, with your own value. Never type the brackets:
in a shell, `<` and `>` have special meanings, and typing them causes confusing errors such
as `The system cannot find the file specified`.

A `#` starts a **comment** in bash. Everything after it on that line is a note for you,
ignored by the computer:

```sh
ls -l        # list the files here, with details
```

## Files and folders (paths)

A **path** is the address of a file or folder. Windows and Linux write them differently:

| | Windows | Linux (bash) |
|---|---|---|
| Separator | backslash `\` | forward slash `/` |
| Example | `C:\Users\Mei\MyGame` | `/home/mei/otomo` |
| Your home folder | `C:\Users\Mei` or `%USERPROFILE%` | `/home/mei`, shortened to `~` |
| Case | `Scenes` and `scenes` are the same | `Scenes` and `scenes` are **different** |

That last row matters for Godot: a game that loads `res://Scenes/Map.tscn` works on Windows
even if the folder is really called `scenes`, but breaks in a Linux build.

**Inside WSL**, your Windows drives are under `/mnt/`: `C:\gs-export` is `/mnt/c/gs-export`.

The shell always has a **current folder** (the **working directory**). Commands work there
unless you give a full path. A path that doesn't start with `/` (or `C:\`) is **relative**:
it's taken from the current folder. Two shorthands help:

- `.` means "this folder".
- `..` means "the folder above this one".

## The commands you'll need

These work in bash (WSL and the server). The ones marked with a star also work in
PowerShell.

| Command | What it does | Example |
|---|---|---|
| `pwd` | Prints the current folder | `pwd` |
| `ls` ★ | Lists what's in a folder | `ls`, `ls -l` (with sizes and dates) |
| `cd` ★ | Changes the current folder | `cd ~/otomo/deploy`, `cd ..` |
| `mkdir` ★ | Makes a new folder | `mkdir build` (bash `-p` also makes missing parent folders) |
| `cat` ★ | Prints a text file | `cat README.md` |
| `cp` ★ | Copies a file | `cp a.txt b.txt` |
| `rm` ★ | Deletes (`-r` a whole folder) | `rm old.txt` |
| `tar` | Packs files into one archive, or unpacks it | see below |

!!! danger "`rm` doesn't use the Recycle Bin"
    Files deleted with `rm` are gone immediately. Check the path twice, especially with
    `rm -rf` ("delete this folder and everything in it, without asking").

**Tab completion** saves typing and prevents typos: type the start of a file or folder
name and press <kbd>Tab</kbd>. The shell completes it if there's exactly one match; press <kbd>Tab</kbd>
twice to list all matches.

### Packing files: tar

`tar` bundles files and folders into one **archive** file (like a `.zip`), which is easier
to copy:

```sh
# Create (c) a gzip-compressed (z) archive file (f) named /tmp/build.tgz from two items:
tar czf /tmp/build.tgz gameserver.x86_64 data_gameserver_linuxbsd_x86_64

# Extract (x) it into the folder "build":
tar xzf /tmp/build.tgz -C build
```

## Running as administrator: sudo

On Linux, some actions need administrator rights, for example reading secret files. You
get them by putting `sudo` ("superuser do") in front of a command:

```sh
sudo cat ~/otomo/deploy/secrets/admin_auth/root_password
```

The first time, it asks for *your* password. Nothing appears on screen while you type it,
not even dots. That's normal: type it and press **Enter**.

## Connecting to the server: SSH

**SSH** (Secure Shell) opens a terminal on *another* computer, over an encrypted connection.
Once connected, every command you type runs on the server, not on your PC.

```sh
ssh you@play.example.com
```

This means "log in as the user `you` on the computer `play.example.com`". The prompt
changes to show you're on the server, for example `you@my-vm:~$`. Type `exit` to come
back to your own computer.

### Keys instead of passwords

The server doesn't accept passwords. It recognises you by an **SSH key**, which comes as a
pair of files:

- a **private key**, which stays on your computer and is never shared;
- a **public key**, which goes on the server. If you run the server, you add it
  yourself; otherwise you send it to whoever does.

When you connect, SSH proves you hold the private key without ever sending it. To make a
key pair, in WSL:

```sh
ssh-keygen -t ed25519
```

Press **Enter** to accept the suggested file location. You can set a passphrase (a password
that protects the key file) or leave it empty. Then print your **public** key and send
the output to whoever manages the server:

```sh
cat ~/.ssh/id_ed25519.pub
```

It's one line starting with `ssh-ed25519`. The file *without* `.pub` is your private key:
never send that one.

### The first connection

The first time you connect to a server, SSH shows its **fingerprint** and asks
`Are you sure you want to continue connecting (yes/no)?`. This is SSH making sure you're
talking to the real server. Type `yes` and press **Enter**. SSH remembers the answer.

### SSH tunnels: reaching private pages

Some of otomo, like the admin website, only listens on the server's own `127.0.0.1` address
(see [How games go online](how-games-go-online.md)), so it's unreachable from the internet.
An **SSH tunnel** carries a port on your computer through your SSH connection to a port on
the server:

```sh
ssh -N -L 8090:127.0.0.1:8090 you@play.example.com
```

- `-L 8090:127.0.0.1:8090` means: "when something on **my** computer connects to port
  8090, carry it through the tunnel to `127.0.0.1:8090` **on the server**".
- `-N` means: "don't open a terminal on the server, just keep the tunnel open".

The command then seems to hang. That's correct: the tunnel is open for as long as the command
runs. Leave that window open, and open `http://localhost:8090/admin/` in your browser. Press
<kbd>Ctrl</kbd>+<kbd>C</kbd> in the terminal to close the tunnel.

### Copying files to the server: scp

`scp` (secure copy) copies files over SSH:

```sh
scp /tmp/build.tgz you@play.example.com:/tmp/
```

"Copy my `/tmp/build.tgz` to the folder `/tmp/` on the server." The part before the `:` is
the server; the part after it is the path *on the server*.

## Stopping and escaping

| Situation | What to press |
|---|---|
| A command is running and you want to stop it | <kbd>Ctrl</kbd>+<kbd>C</kbd> |
| The screen shows a long file and `:` at the bottom (a *pager*) | <kbd>Q</kbd> |
| You ran `nano` (a text editor) | <kbd>Ctrl</kbd>+<kbd>X</kbd>, then <kbd>Y</kbd> to save or <kbd>N</kbd> not to |
| You're on the server and want to go back to your PC | type `exit` |

## Next

That's all the Concepts pages. Go to [Install the SDK](../guide/install-the-sdk.md) to put
otomo into your game.
