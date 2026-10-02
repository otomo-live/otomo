# HTTP and APIs

Almost everything your game says to otomo, apart from the match itself, is an **HTTP
request**. HTTP is the same language your web browser uses to load pages. This page explains
what's inside a request and a response, what the numbers like `404` mean, and what people
mean by "API" and "REST". You'll need it to read otomo's logs and error messages.

!!! tip "You won't write HTTP code yourself"
    The SDK does all of this for you. This page is here so the SDK's log lines and errors
    make sense, and so you can check the server by hand when something's wrong.

## A conversation in two parts

HTTP is strictly question-and-answer. The client sends a **request**; the server sends back
exactly one **response**. The server never speaks first.

Here's a real request your game sends when it logs in, exactly as it travels:

```http
POST /auth/anonymous HTTP/1.1
Host: play.example.com
Content-Type: application/json

{"device_id":"q3JqN1mG4c8u9xYwT0bVn5dKs2LpR7eHf6AzWiUoQyE"}
```

And the server's response:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"access_token":"eyJhbGciOi…","expires_in":900,"refresh_token":"b0f3…"}
```

Each part is explained below.

## The request

### The method: what you want to do

The first word, `POST`, is the **method**. It says what kind of action the request is.
Otomo uses four:

| Method | Meaning | Example |
|---|---|---|
| `GET` | "Give me this." Reads something and changes nothing. | Get the list of content files |
| `POST` | "Do this / create this." | Log in, create a party, launch a match |
| `PATCH` | "Change part of this." | Change the party's difficulty setting |
| `DELETE` | "Remove this." | Remove a friend |

### The path: which thing

`/auth/anonymous` is the **path**: which thing on the server you mean, much like a file path.
Put the server's address in front of it and you get a full **URL** (web address):

```
https://play.example.com/auth/anonymous
└─┬──┘   └───────┬───────┘└──────┬──────┘
scheme       host name          path
```

- The **scheme** (`https`) says how to talk: HTTP inside TLS encryption.
- The **host name** says which computer ([DNS](how-games-go-online.md) turns it into an IP
  address).
- The **path** says which thing on that computer.

Sometimes a path ends with a **query**: extra settings after a `?`, such as
`/events?after=41`, which means "events after number 41".

### Headers: notes on the envelope

The lines after the first one, like `Content-Type: application/json`, are **headers**. Each
is a `Name: value` pair that tells the other side something *about* the message. The ones
you'll meet most in otomo:

| Header | Meaning |
|---|---|
| `Content-Type: application/json` | "The body is JSON" (see below) |
| `Authorization: Bearer eyJ…` | "Here's my login pass" ([Logins and tokens](logins-and-tokens.md)) |
| `X-Request-Id: 0192f3a4-…` | A unique ID for this request, used to find it in the server's logs |
| `ETag` / `If-None-Match` | "Has this changed since I last asked?" ([Content and patching](content-and-patching.md)) |

### The body: the actual data

After an empty line comes the **body**: the data itself. A `GET` usually has none. A `POST`
usually carries the details, here the device ID.

## JSON: the format of the data

Otomo's bodies are **JSON**, a plain-text way of writing data that's easy for both people
and programs to read. You'll also write JSON yourself in the admin website, so it's worth
learning the five rules:

```json
{
  "text": "Double XP weekend!",
  "show": true,
  "xp_multiplier": 2.0,
  "regions": ["asia", "europe"],
  "style": { "color": "#66ccff" }
}
```

1. `{ … }` is an **object**: a set of `"name": value` pairs separated by commas. Names are
   always in double quotes.
2. Text (a **string**) is in double quotes: `"Double XP weekend!"`.
3. Numbers have no quotes: `2.0`, `100`.
4. `true`, `false` and `null` are written as-is, without quotes.
5. `[ … ]` is a **list** (JSON calls it an array): values separated by commas.

Objects and lists can contain each other, as `"style"` does above.

!!! warning "The two most common JSON mistakes"
    - **A comma after the last item** (`"show": true,}`) is not allowed.
    - **Single quotes** (`'text'`) aren't allowed: always use double quotes.

    The admin website checks your JSON and points at the mistake, but it's quicker to
    avoid these in the first place.

## The response

### The status code: how it went

The first line of the response, `HTTP/1.1 200 OK`, holds the **status code**, a three-digit
number that tells you at a glance how it went. The first digit is the category:

| Codes | Category | Meaning |
|---|---|---|
| **2xx** | Success | It worked. |
| **3xx** | Go elsewhere | Nothing to send, or look somewhere else. |
| **4xx** | *Your* mistake | The request was wrong, or you're not allowed. Retrying the same thing won't help. |
| **5xx** | *The server's* problem | Something broke on the server side. Trying again later may work. |

The codes you'll actually meet with otomo:

| Code | Name | In otomo, it usually means |
|---|---|---|
| 200 | OK | Here's what you asked for. |
| 201 | Created | Done, something new was made (a party, an allocation). |
| 202 | Accepted | Started; the result will come later (a launch). |
| 204 | No Content | Done, nothing to send back. |
| 206 | Partial Content | Here's the *rest* of a file you'd partly downloaded. |
| 304 | Not Modified | Nothing changed since you last asked; use your copy. |
| 400 | Bad Request | Something in the request is malformed or invalid. |
| 401 | Unauthorized | Your login pass is missing, expired or invalid. |
| 403 | Forbidden | Your pass is fine, but you're not allowed to do *this*. |
| 404 | Not Found | That thing doesn't exist (or you're not in a party). |
| 409 | Conflict | The request clashes with the current state (someone changed the party first, your content is out of date). |
| 429 | Too Many Requests | Slow down; you're asking too often. |
| 500 | Internal Server Error | A bug on the server. |
| 502 / 503 | Bad Gateway / Unavailable | A server behind the front door is down or still starting. |

### The error body

When otomo answers with a 4xx or 5xx, the body always has the same shape:

```json
{"error":{"code":"not_in_party","message":"you are not in a party","request_id":"01a0ecdd-ed6b-…"}}
```

- `code` is a short, fixed word your code can check (`not_in_party`).
- `message` is a sentence for humans.
- `request_id` identifies this exact request in the server's logs. **Always include it when
  you report a problem**: with it, whoever looks at the server finds your request in
  seconds.

[Error codes](../reference/errors.md) lists every code and what to do about it.

## What "API" and "REST" mean

An **API** (Application Programming Interface) is the list of requests a server
understands: which methods and paths exist, what goes in the body, and what comes back.
"Otomo's API" is simply the set of requests your game can make to it.

**REST** is a common *style* for designing HTTP APIs. The idea is to name *things* in the
path and use the method to say what to do with them:

| Request | Meaning |
|---|---|
| `GET /party` | Read my party |
| `POST /party` | Create a party |
| `POST /party/ready` | Mark me ready |
| `PATCH /party/settings` | Change some of the party's settings |

If you've ever called a function like `party.GetSettings()`, a REST API is the same idea
over the network: the path is the object, and the method is the verb.

## Trying it yourself

You can send HTTP requests by hand with **curl**, a small program built into Windows 10
and 11, macOS and Linux. Open a terminal (PowerShell on Windows, or WSL) and type:

```sh
curl -i https://play.example.com/patch/v1/live/manifest
```

- `curl` sends a `GET` request to the URL.
- `-i` asks it to also print the status line and headers, not just the body.

You should see `HTTP/1.1 200 OK`, some headers, and then a JSON body describing the current
content release. If you do, otomo is up and reachable from your computer.

!!! note "PowerShell's `curl` is sometimes a different program"
    In older Windows PowerShell, `curl` is a nickname for a different command that
    behaves differently. If the output looks nothing like the above, type `curl.exe`
    instead of `curl`.

## Next

Continue with [Logins and tokens](logins-and-tokens.md): what that `Authorization: Bearer`
header is, and why the game logs in without the player typing a password.
