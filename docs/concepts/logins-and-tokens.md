# Logins and tokens

Players never type a username or password into a game built with otomo, yet every one of
them has their own account. This page explains how that works, what a **token** is, and
what the SDK does when a login runs out. You don't need to write any of this. The SDK
handles it, but it explains messages like `401` and `AuthenticationLost`.

## Why a server needs to know who you are

A server receives requests from thousands of games at once. When one of them says "put me
in a party", the server must know *who* "me" is, and must be sure it isn't someone
pretending. Proving who you are is called **authentication**, usually shortened to
**auth**. Otomo's **Auth** service does it.

## The player's identity: a device ID

The first time your game runs on a computer, the SDK makes up a long random string, the
**device ID**, for example `q3JqN1mG4c8u9xYwT0bVn5dKs2LpR7eHf6AzWiUoQyE`, and saves it in
the game's user data folder. That string *is* the account. Whenever the game shows it to
Auth, Auth answers "ah, that player".

This is called an **anonymous** or **device** login: no email, no password, no sign-up
screen. It's how many mobile and casual games work.

!!! danger "The device ID is the account"
    Anyone who copies another player's device ID file becomes that player. The SDK never
    prints it in logs, and you should never ask players to send it. If a player deletes
    their user data folder, the SDK makes a new ID, which means a **new, empty account**.
    Otomo has no way to recover the old one yet.

## Tokens: a pass instead of a password

Sending the device ID with every single request would be risky: the more often a secret
travels, the more chances it has to leak. So the game shows it **once**, and Auth hands back
a **token** in exchange.

A token is a piece of text that works like a wristband at a festival: you show your ticket
once at the gate, get a wristband, and from then on you just show the wristband. The
wristband says who you are, it's hard to fake, and it expires.

Otomo gives the game two tokens:

| Token | Lasts | Used for | Kept where |
|---|---|---|---|
| **Access token** | 15 minutes | Attached to every request: "this is player X" | Only in memory, never saved |
| **Refresh token** | 30 days | Getting a new access token when the old one expires | Saved in `user://otomo/refresh_token` |

### How the access token travels

The game attaches the access token to each request in a header (see
[HTTP and APIs](http-and-apis.md)):

```http
GET /api/player/session/me HTTP/1.1
Authorization: Bearer eyJhbGciOiJFZERTQSIsImtpZCI6Ij…
```

"Bearer" means "whoever carries this token is allowed in", which is exactly why tokens must
be kept secret.

### What's inside a token

The access token is a **JWT** (JSON Web Token, often pronounced "jot"). It's three pieces
of text joined by dots: a header, the **claims** (the facts it states, such as the player's
ID and the expiry time), and a **signature**.

The signature is the clever part. Auth signs every token with a secret key only it knows.
Anyone can *check* the signature using Auth's public key, but nobody can *make* a valid one
without the secret. So when otomo's other services receive a token, they check it
themselves in microseconds, without asking Auth, and they know it's genuine.

## What happens when a token expires

Fifteen minutes is short on purpose: if an access token ever leaks, it's useless soon. The
SDK handles expiry for you:

1. The game makes a request with an expired access token.
2. The server answers **`401 Unauthorized`**.
3. The SDK sends the refresh token to Auth and gets a **new** access token *and* a **new**
   refresh token. The old refresh token stops working at that moment.
4. The SDK saves the new refresh token and repeats the original request, which now works.

Your game code never sees steps 2 to 4. It just gets its answer, slightly later.

!!! note "Why the refresh token changes every time"
    Every refresh token works exactly once. If a stolen refresh token is ever used after
    the real game has already used it, Auth notices the reuse and cancels the whole chain
    of tokens. The thief is locked out, and the real player simply logs in again with
    their device ID.

### When the SDK gives up

If a request still gets `401` *after* a refresh, the login can't be repaired in the
background: for example, the server cancelled the token chain, or the server's signing key
changed. The SDK then raises the `AuthenticationLost`
signal. Your game should go back to its start-up screen and log in again (calling
`StartAsync` again does that). See the [SDK reference](../reference/sdk.md).

## 401 versus 403

These two codes look similar but mean different things, and mixing them up causes bugs:

| Code | Means | What the SDK does |
|---|---|---|
| **401 Unauthorized** | "I don't know who you are": the token is missing, expired or invalid | Refreshes once and retries once |
| **403 Forbidden** | "I know who you are, and you're not allowed to do this" | Nothing: a new token wouldn't change the answer |

For example, a player who isn't the party leader and tries to launch the match gets `403`
with the code `not_leader`. Refreshing wouldn't make them the leader.

## Staff accounts are separate

The people who use the admin website log in differently: with an email, a password and,
usually, a second factor from an authenticator app. They get *staff* tokens from a
separate service. A player token is never accepted by the admin side, and a staff token is
never accepted by the game side, so even a leaked token can't cross over. See
[Use the admin website](../guide/admin-website.md).

## Where the SDK keeps things

In the game's user data folder (in Godot: **Project → Open User Data Folder**), under
`otomo/`:

| File | What it is | Safe to delete? |
|---|---|---|
| `device_id` | The player's identity | Only if you want a **new account** |
| `refresh_token` | The saved login | Yes; the game logs in again with the device ID |
| `patch/` | Downloaded content ([Content and patching](content-and-patching.md)) | Yes; the game downloads it again |

## Next

Continue with [How otomo fits together](how-otomo-fits-together.md), a tour of every
otomo service.
