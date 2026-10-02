// SE-9: 2,000 clients heartbeating and long-polling Session through the
// player gateway, the steady state of a full game night.
//
//   k6 run -e GATEWAY=http://127.0.0.1:8080 services/session/load/presence-longpoll.js
//
// Each client is one player: it logs in once (POST /auth/anonymous with its own device
// id), calls /me/init, then two scenarios run side by side for the same 2,000 players:
//
//   heartbeat  one POST /presence/heartbeat every 20 s per player (the SDK's rhythm)
//   longpoll   GET /events?after=<cursor> back to back per player (held up to 25 s)
//
// Thresholds are SE-9's acceptance criteria: p95 heartbeat under 50 ms, and no failed
// requests. Goroutines are read from Session's go_goroutines before and after (see
// load/README.md); k6 cannot reach the metrics port.
//
// The player gateway limits each client IP (GATEWAY_RATE_LIMIT_RPS 20, login 5 a
// second). Run from one machine only with those raised for the test window, as
// load/README.md describes, or every run ends in 429s.

import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Trend } from 'k6/metrics';

const GATEWAY = __ENV.GATEWAY || 'http://127.0.0.1:8080';
const CLIENTS = parseInt(__ENV.CLIENTS || '2000', 10);
const DURATION = __ENV.DURATION || '10m';
const RAMP = __ENV.RAMP || '2m';
const SESSION = `${GATEWAY}/api/player/session`;

const heartbeat = new Trend('session_heartbeat_ms', true);
const poll = new Trend('session_poll_ms', true);

export const options = {
  scenarios: {
    heartbeat: {
      executor: 'ramping-vus',
      exec: 'beat',
      startVUs: 0,
      stages: [
        { duration: RAMP, target: CLIENTS },
        { duration: DURATION, target: CLIENTS },
      ],
      gracefulRampDown: '5s',
    },
    longpoll: {
      executor: 'ramping-vus',
      exec: 'longPoll',
      startVUs: 0,
      stages: [
        { duration: RAMP, target: CLIENTS },
        { duration: DURATION, target: CLIENTS },
      ],
      // A held poll may take the full 25 s to answer.
      gracefulRampDown: '30s',
    },
  },
  thresholds: {
    session_heartbeat_ms: ['p(95)<50'],
    http_req_failed: ['rate<0.001'],
    checks: ['rate>0.999'],
  },
};

// Player n of the run is the same player in both scenarios: VU ids are numbered across
// the whole test, so heartbeat VU n and long-poll VU n + CLIENTS share a device id.
function deviceID() {
  const n = (exec.vu.idInTest - 1) % CLIENTS;
  return `k6-${__ENV.RUN || 'load'}-player-${String(n).padStart(8, '0')}`;
}

// login runs once per VU and keeps the token for the VU's lifetime. Access tokens last
// 15 minutes, longer than the default run.
function login(state) {
  if (state.token) {
    return state.token;
  }
  const res = http.post(`${GATEWAY}/auth/anonymous`, JSON.stringify({ device_id: deviceID() }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'login' },
  });
  check(res, { 'login 200': (r) => r.status === 200 });
  state.token = res.json('access_token');
  const init = http.post(`${SESSION}/me/init`, null, {
    headers: { Authorization: `Bearer ${state.token}` },
    tags: { name: 'me_init' },
  });
  check(init, { 'me/init 200': (r) => r.status === 200 });
  return state.token;
}

const vu = {};

export function beat() {
  const token = login(vu);
  const res = http.post(`${SESSION}/presence/heartbeat`, JSON.stringify({ status: 'online' }), {
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    tags: { name: 'heartbeat' },
  });
  heartbeat.add(res.timings.duration);
  check(res, { 'heartbeat 204': (r) => r.status === 204 });
  sleep(20);
}

export function longPoll() {
  const token = login(vu);
  const res = http.get(`${SESSION}/events?after=${vu.cursor || 0}`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: 'events' },
    timeout: '40s',
  });
  poll.add(res.timings.duration);
  check(res, { 'events 200': (r) => r.status === 200 });
  if (res.status !== 200) {
    sleep(1);
    return;
  }
  const body = res.json();
  if (Array.isArray(body) && body.length > 0) {
    vu.cursor = body[body.length - 1].seq;
  } else if (body && body.resync) {
    vu.cursor = 0;
  }
}
