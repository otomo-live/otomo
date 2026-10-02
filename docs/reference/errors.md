# Error codes

When something goes wrong, otomo and the SDK report a short, fixed **code**: a word such as
`network` or `not_in_party`. You'll find it in `OtomoError.Code` in your C# code, in the
`errorCode` of the `PatchFinished` signal, and in the SDK's log lines, like this:

```text
GET /api/player/session/party -> 404 not_in_party request_id=01a0ecdd-ed6b-7df5-8025-9701e4d593d0
```

That line says: the request `GET /api/player/session/party` got status `404` with code
`not_in_party`, and its ID in the server logs is `01a0ecdd-…`. Status codes are explained
in [HTTP and APIs](../concepts/http-and-apis.md).

!!! tip "Reporting a problem"
    Always include the **`request_id`**. With it, whoever checks the server finds your
    exact request in the admin website's **Logs** page in seconds. Without it, they're
    guessing.

## From the SDK itself (status 0)

These happen on the player's computer, before or without a server answer. `Status` is `0`.

| Code | Meaning | What to do |
|---|---|---|
| `network` | Couldn't connect to otomo: no internet, wrong **Base Url**, or the server is down | Check the internet connection and the Base Url setting. The game carries on offline with its last content. |
| `timeout` | The server took too long to answer | Usually temporary; try again. If it keeps happening, the server may be overloaded. |
| `bad_response` | The server answered with something the SDK couldn't read | Usually a mismatch between SDK and server versions. Report it with the `request_id`. |
| `bad_manifest` | The content list from Patch is malformed or unsafe | Report it; the SDK keeps the last good content. |
| `missing_blob` | A file the release needs wasn't on disk when the SDK went to load it | Usually the download was interrupted or the files were deleted mid-run. Start the game again; it downloads what's missing. |
| `size_mismatch` | A download came out bigger than the release says the file is | Try again. If it repeats, report it with the release number. |
| `pack_mount_failed` | Godot couldn't load a downloaded pack | The `.pck` may be for another Godot version, or damaged. Rebuild and re-upload it. |
| `download_declined` | The player said no to the download | Not an error: the game carries on with its old content. |
| `unknown` | The server answered with an error but without otomo's usual error details | Check the `Status`; report it with the `request_id`. |

## Logins (any request)

| Status | Code | Meaning | What to do |
|---|---|---|---|
| 401 | `missing_token` | The request had no login token | The SDK adds tokens itself. If you send requests by hand, include `Authorization: Bearer …`. |
| 401 | `expired` | The access token has expired | The SDK renews it and retries automatically. |
| 401 | `invalid_token`, `invalid_signature`, `iss_mismatch`, `aud_mismatch` | The token isn't valid for otomo | The SDK logs in again. If it persists, `AuthenticationLost` fires. |
| 403 | `insufficient_role` | A staff account lacks the role for this action | Ask an admin for a higher role ([Use the admin website](../guide/admin-website.md)). |
| 400 | `validation_failed` (at `/auth/…`) | The saved device ID is malformed, usually because someone edited the file by hand | Delete `otomo/device_id` in the user data folder. This creates a **new account**. |
| 429 | `rate_limit_exceeded` | Too many requests too quickly | Wait a moment. If your code causes this, look for a loop sending requests every frame. |

## Content

| Status | Code | Meaning | What to do |
|---|---|---|---|
| 409 | `release_outdated` | The game's content release isn't the current one (your team published or rolled back while it was running) | Run patching again, then retry. The SDK will do this automatically in a future version. |
| 400 | `invalid_release` | The release number the game sent isn't a valid number | A bug in the calling code. |

## Profiles, friends and parties (Session)

| Status | Code | Meaning |
|---|---|---|
| 404 | `not_in_party` | The player isn't in a party. Normal before creating one. |
| 409 | `already_in_party` | The player must leave their current party first |
| 403 | `not_leader` | Only the party leader can do this |
| 409 | `revision_mismatch` | Someone changed the party first. Fetch it again and retry ([Parties and matches](../concepts/parties-and-matches.md)). |
| 400 | `revision_required` | The leader's request didn't include the party's revision number |
| 409 | `party_locked` | The party is launching or in a match; only leaving is allowed |
| 409 | `party_full` | The party has reached its maximum size |
| 409 | `not_ready` | Launch refused: not every member is ready |
| 400 / 409 | `invalid_settings` | A lobby setting or value isn't allowed by `session.rules` |
| 409 | `not_in_game` | Asked for a join ticket, but the party isn't in a match |
| 409 | `match_ended` | The match is over; launch a new one |
| 404 | `not_in_match` | The player isn't part of that match |
| 503 | `allocator_unavailable` | Session couldn't reach the Allocator; try again shortly |
| 503 | `launch_unavailable` | Launching isn't switched on for this server |
| 409 | `name_unavailable` | That display name is taken or not allowed |
| 400 | `invalid_display_name` | The name breaks the naming rules (length or characters) |
| 409 | `already_friends` / `friend_limit` | Already friends / the friends list is full |
| 403 | `blocked` | One player has blocked the other |
| 404 | `player_not_found`, `profile_not_found`, `not_friends`, `invite_not_found`, `request_not_found` | The player, profile, friendship, invite or friend request doesn't exist |
| 410 | `invite_expired` | The party invite is too old |

## Joining a match (Gameplay Proxy)

The proxy speaks UDP, not HTTP, so these arrive as a reason number, and the launch code
turns them into a message.

| Reason | Message | Meaning | What to do |
|---|---|---|---|
| 1 | `invalid` | The ticket is damaged, forged, or the proxy is full | Join again |
| 2 | `expired` | The ticket is older than 60 seconds | Join again; a fresh ticket is fetched |
| 3 | `already used` | This ticket was already used from another address | Join again |
| 4 | `unknown server` | The game server in the ticket isn't registered | Join again; if it repeats, check the game servers ([Run a new game server build](../guide/game-servers.md)) |

## Anywhere

| Status | Code | Meaning |
|---|---|---|
| 400 | `validation_failed`, `invalid_body`, `invalid_request` | Something in the request is missing or malformed. `message` says what. |
| 404 | `not_found` | No such route: check the path |
| 405 | `method_not_allowed` | The path exists, but not with that method (`GET` instead of `POST`, for example) |
| 413 | `body_too_large` | The request body is too big |
| 500 | `internal_error` | A bug on the server. Report it with the `request_id`. |
| 502 | `upstream_error` | The gateway couldn't reach the service behind it |
| 503 | `not_ready` | The service is starting up or has lost its database; try again in a moment |
