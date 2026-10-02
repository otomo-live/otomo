# SE-9: Session smoke run through the player gateway, against the deployed
# stack. Two new anonymous players go through every player feature once.
#
#   hurl --test --variable gateway=http://127.0.0.1:8080 services/session/smoke/session.hurl
#
# Needs Hurl 4 or later (for newUuid) and a stack that deploy/scripts/up.sh started.
# Each run makes two new players, so it can be repeated. With no game servers the
# launch ends in party.launch_failed and the lobby is forming again; with them it is
# in_game. Either passes.

# ---- Player A logs in and gets a profile -------------------------------------------

POST {{gateway}}/auth/anonymous
{"device_id": "{{newUuid}}"}
HTTP 200
[Captures]
token_a: jsonpath "$.access_token"

POST {{gateway}}/api/player/session/me/init
Authorization: Bearer {{token_a}}
HTTP 200
[Captures]
a: jsonpath "$.player_id"
[Asserts]
jsonpath "$.display_name" isString
jsonpath "$.discriminator" isInteger

GET {{gateway}}/api/player/session/me
Authorization: Bearer {{token_a}}
HTTP 200
[Asserts]
jsonpath "$.player_id" == "{{a}}"

# ---- Player B -----------------------------------------------------------------------

POST {{gateway}}/auth/anonymous
{"device_id": "{{newUuid}}"}
HTTP 200
[Captures]
token_b: jsonpath "$.access_token"

POST {{gateway}}/api/player/session/me/init
Authorization: Bearer {{token_b}}
HTTP 200
[Captures]
b: jsonpath "$.player_id"
b_name: jsonpath "$.display_name"
b_disc: jsonpath "$.discriminator"

# ---- Presence (SE-3) ----------------------------------------------------------------

POST {{gateway}}/api/player/session/presence/heartbeat
Authorization: Bearer {{token_a}}
{"status": "online"}
HTTP 204

# A second beat inside 10 s is refused.
POST {{gateway}}/api/player/session/presence/heartbeat
Authorization: Bearer {{token_a}}
{"status": "away"}
HTTP 429
[Asserts]
header "Retry-After" exists
jsonpath "$.error.code" == "rate_limit_exceeded"

POST {{gateway}}/api/player/session/presence/heartbeat
Authorization: Bearer {{token_a}}
{"status": "busy"}
HTTP 400
[Asserts]
jsonpath "$.error.code" == "invalid_status"

# ---- Friends and blocks (SE-5) ------------------------------------------------------

POST {{gateway}}/api/player/session/friends/requests
Authorization: Bearer {{token_a}}
{"display_name": "{{b_name}}", "discriminator": {{b_disc}}}
HTTP 200
[Asserts]
jsonpath "$.player_id" == "{{b}}"
jsonpath "$.state" == "pending"

GET {{gateway}}/api/player/session/friends
Authorization: Bearer {{token_b}}
HTTP 200
[Asserts]
jsonpath "$.incoming" count == 1
jsonpath "$.incoming[0].player_id" == "{{a}}"

POST {{gateway}}/api/player/session/friends/requests/{{a}}/accept
Authorization: Bearer {{token_b}}
HTTP 200
[Asserts]
jsonpath "$.state" == "accepted"

# A is online and B has not beaten, so B sees A online and A sees B offline.
GET {{gateway}}/api/player/session/friends
Authorization: Bearer {{token_b}}
HTTP 200
[Asserts]
jsonpath "$.friends" count == 1
jsonpath "$.friends[0].status" == "online"

GET {{gateway}}/api/player/session/friends
Authorization: Bearer {{token_a}}
HTTP 200
[Asserts]
jsonpath "$.friends[0].player_id" == "{{b}}"
jsonpath "$.friends[0].status" == "offline"

# ---- Party and lobby (SE-6, LB-2) ---------------------------------------------------

POST {{gateway}}/api/player/session/party
Authorization: Bearer {{token_a}}
HTTP 201
[Captures]
party: jsonpath "$.party_id"
[Asserts]
jsonpath "$.state" == "forming"
jsonpath "$.leader_id" == "{{a}}"

GET {{gateway}}/api/player/session/party
Authorization: Bearer {{token_a}}
HTTP 200
[Captures]
rev: jsonpath "$.revision"
[Asserts]
jsonpath "$.members[0].status" == "online"

# The event stream has the party.updated for the new party (SE-4); it answers at once.
GET {{gateway}}/api/player/session/events?after=0
Authorization: Bearer {{token_a}}
HTTP 200
[Asserts]
jsonpath "$[*].type" includes "party.updated"

POST {{gateway}}/api/player/session/party/ready
Authorization: Bearer {{token_a}}
{"ready": true}
HTTP 200
[Captures]
rev: jsonpath "$.revision"
[Asserts]
jsonpath "$.members[0].ready" == true

# A leader call without a revision.
POST {{gateway}}/api/player/session/party/launch
Authorization: Bearer {{token_a}}
{}
HTTP 400
[Asserts]
jsonpath "$.error.code" == "revision_required"

# ---- Launch (LB-3) ------------------------------------------------------------------

POST {{gateway}}/api/player/session/party/launch
Authorization: Bearer {{token_a}}
{"revision": {{rev}}}
HTTP 202
[Asserts]
jsonpath "$.state" == "launching"

# Launching ends within the Allocator's 5 s deadline: forming again (no_capacity) or
# in_game.
GET {{gateway}}/api/player/session/party
Authorization: Bearer {{token_a}}
[Options]
retry: 10
retry-interval: 1000
HTTP 200
[Asserts]
jsonpath "$.state" != "launching"

# ---- Blocks -------------------------------------------------------------------------

POST {{gateway}}/api/player/session/blocks/{{a}}
Authorization: Bearer {{token_b}}
HTTP 204

GET {{gateway}}/api/player/session/friends
Authorization: Bearer {{token_b}}
HTTP 200
[Asserts]
jsonpath "$.friends" count == 0

POST {{gateway}}/api/player/session/friends/requests
Authorization: Bearer {{token_a}}
{"display_name": "{{b_name}}", "discriminator": {{b_disc}}}
HTTP 403
[Asserts]
jsonpath "$.error.code" == "blocked"

DELETE {{gateway}}/api/player/session/blocks/{{a}}
Authorization: Bearer {{token_b}}
HTTP 204

# ---- Leaving ------------------------------------------------------------------------

POST {{gateway}}/api/player/session/party/leave
Authorization: Bearer {{token_a}}
HTTP 204

GET {{gateway}}/api/player/session/party
Authorization: Bearer {{token_a}}
HTTP 404
[Asserts]
jsonpath "$.error.code" == "not_in_party"

# ---- Boundaries (SES-A1) ------------------------------------------------------------

GET {{gateway}}/api/player/session/me
HTTP 401

# A player token on a staff route. The gateway's own guard may answer first, so only
# the status is checked.
GET {{gateway}}/api/admin/session/audit
Authorization: Bearer {{token_a}}
HTTP *
[Asserts]
status >= 401
status <= 404
