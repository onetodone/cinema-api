// booking_race.js: many users try to book the same seat at the same moment. Exactly one may get it.
//
// Every virtual user (VU) is a separate account that sends one POST /v1/bookings for the same seat of the same
// showtime, all at once. The thresholds pass only if exactly one request gets 201, every other one gets 409, and
// nothing else comes back (no 5xx, no timeouts). Teardown checks the database's view through the API: the seat is
// held, and exactly one of the users has a booking for it; then it cancels that booking, so the run leaves the
// seat as it found it.
//
// Run it against a running API (see the README, "Load test"):
//
//   make load-test                                  # k6 from PATH, or the grafana/k6 image
//   k6 run -e BASE_URL=http://localhost:8080 test/load/booking_race.js
//
// Environment:
//   BASE_URL     the API, default http://localhost:8080
//   VUS          contenders, default 500
//   SHOWTIME_ID  the showtime to race for; default: the first showtime of today or tomorrow that starts in more
//                than 10 minutes and has a free seat
//   SEAT_ID      the seat to race for; default: the first available seat of the showtime
//   KEEP=1       keep the winning booking instead of canceling it in teardown
//
// Setup registers VUS accounts (k6-race-<n>@load.test; existing ones are reused) and logs each one in, so the API
// must run without the per-address auth limit, and a lower bcrypt cost makes setup faster:
//   make run-api AUTH_IP_RATE_LIMIT_PER_MIN=0 BCRYPT_COST=10

import http from 'k6/http';
import { check, fail } from 'k6';
import { Counter } from 'k6/metrics';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/+$/, '');
const VUS = parseInt(__ENV.VUS || '500', 10);
const PASSWORD = 'k6-race-password';
const BATCH = 50; // requests per http.batch call in setup and teardown
const JSON_HEADERS = { 'Content-Type': 'application/json' };

const created = new Counter('bookings_created');
const rejected = new Counter('bookings_rejected');
const unexpected = new Counter('unexpected_responses');
const winners = new Counter('winning_bookings_found');

export const options = {
  setupTimeout: '5m',
  teardownTimeout: '2m',
  scenarios: {
    race: {
      executor: 'per-vu-iterations', // every VU sends its one request as soon as the scenario starts
      vus: VUS,
      iterations: 1,
      maxDuration: '1m',
    },
  },
  thresholds: {
    bookings_created: ['count==1'],
    bookings_rejected: [`count==${VUS - 1}`],
    unexpected_responses: ['count==0'],
    winning_bookings_found: ['count==1'],
    'http_req_duration{name:book}': ['p(95)<3000'],
  },
  summaryTrendStats: ['min', 'med', 'p(95)', 'p(99)', 'max'],
};

function url(path) {
  return BASE_URL + path;
}

function auth(token) {
  return { headers: { ...JSON_HEADERS, Authorization: `Bearer ${token}` } };
}

// batched runs requests in chunks and returns the responses in order.
function batched(requests) {
  const out = [];
  for (let i = 0; i < requests.length; i += BATCH) {
    out.push(...http.batch(requests.slice(i, i + BATCH)));
  }
  return out;
}

function isoDate(d) {
  return d.toISOString().slice(0, 10);
}

// findTarget picks the showtime and seat to race for.
function findTarget() {
  let showtimeId = __ENV.SHOWTIME_ID ? parseInt(__ENV.SHOWTIME_ID, 10) : 0;
  if (!showtimeId) {
    const soon = Date.now() + 10 * 60 * 1000;
    for (const day of [new Date(), new Date(Date.now() + 24 * 3600 * 1000)]) {
      const res = http.get(url(`/v1/showtimes?date=${isoDate(day)}`));
      if (res.status !== 200) fail(`GET /v1/showtimes: ${res.status} ${res.body}`);
      const st = res.json('items').find((s) => Date.parse(s.starts_at) > soon && s.seats_available > 0);
      if (st) {
        showtimeId = st.id;
        break;
      }
    }
    if (!showtimeId) fail('no bookable showtime today or tomorrow; seed the catalog (make seed) or set SHOWTIME_ID');
  }

  const res = http.get(url(`/v1/showtimes/${showtimeId}/seats`));
  if (res.status !== 200) fail(`GET seat map of showtime ${showtimeId}: ${res.status} ${res.body}`);
  const seats = res.json('seats');
  const seat = __ENV.SEAT_ID
    ? seats.find((s) => s.id === parseInt(__ENV.SEAT_ID, 10))
    : seats.find((s) => s.status === 'available');
  if (!seat || seat.status !== 'available') fail(`seat ${__ENV.SEAT_ID || '(any)'} of showtime ${showtimeId} is not available`);
  return { showtimeId, seatId: seat.id, label: `${seat.row}${seat.number}` };
}

// signIn registers the race accounts (an existing account is fine) and returns their access tokens.
function signIn() {
  const creds = [];
  for (let i = 0; i < VUS; i++) {
    creds.push(JSON.stringify({ email: `k6-race-${i}@load.test`, password: PASSWORD }));
  }
  const post = (path, ...expected) =>
    creds.map((body) => ['POST', url(path), body, { headers: JSON_HEADERS, responseCallback: http.expectedStatuses(...expected) }]);

  // 409 EMAIL_TAKEN: the account exists from an earlier run, which is fine.
  batched(post('/v1/auth/register', 201, 409)).forEach((res, i) => {
    if (res.status === 429) fail('registration is rate limited: run the API with AUTH_IP_RATE_LIMIT_PER_MIN=0');
    if (res.status !== 201 && res.status !== 409) fail(`register k6-race-${i}: ${res.status} ${res.body}`);
  });
  return batched(post('/v1/auth/login', 200)).map((res, i) => {
    if (res.status === 429) fail('login is rate limited: run the API with AUTH_IP_RATE_LIMIT_PER_MIN=0');
    if (res.status !== 200) fail(`login k6-race-${i}: ${res.status} ${res.body}`);
    return res.json('access_token');
  });
}

// activeBookings returns, per token, the ids of that user's unpaid bookings for the showtime.
function activeBookings(tokens, showtimeId) {
  const lists = batched(tokens.map((t) => ['GET', url('/v1/bookings?limit=100'), null, auth(t)]));
  return lists.map((res, i) => {
    if (res.status !== 200) fail(`list bookings of k6-race-${i}: ${res.status} ${res.body}`);
    return res
      .json('items')
      .filter((b) => b.showtime.id === showtimeId && (b.status === 'pending' || b.status === 'processing'))
      .map((b) => b.id);
  });
}

// cancel cancels the given bookings, per token.
function cancel(tokens, idsPerToken) {
  const requests = [];
  idsPerToken.forEach((ids, i) => ids.forEach((id) => requests.push(['DELETE', url(`/v1/bookings/${id}`), null, auth(tokens[i])])));
  batched(requests).forEach((res) => {
    if (res.status !== 204) console.warn(`cancel ${res.request.url}: ${res.status} ${res.body}`);
  });
}

export function setup() {
  const target = findTarget();
  console.log(`racing ${VUS} users for seat ${target.label} (id ${target.seatId}) of showtime ${target.showtimeId} at ${BASE_URL}`);
  const tokens = signIn();
  // A booking left over from an interrupted run would make its owner's attempt fail with ACTIVE_BOOKING_EXISTS.
  cancel(tokens, activeBookings(tokens, target.showtimeId));
  return { ...target, tokens };
}

export default function (data) {
  const token = data.tokens[__VU - 1];
  const res = http.post(
    url('/v1/bookings'),
    JSON.stringify({ showtime_id: data.showtimeId, seat_ids: [data.seatId] }),
    { ...auth(token), tags: { name: 'book' }, responseCallback: http.expectedStatuses(201, 409) },
  );

  // Every VU adds to every counter, zero included, so that each threshold has samples to judge.
  created.add(res.status === 201 ? 1 : 0);
  rejected.add(res.status === 409 ? 1 : 0);
  unexpected.add(res.status === 201 || res.status === 409 ? 0 : 1);
  if (res.status !== 201 && res.status !== 409) {
    console.error(`VU ${__VU}: ${res.status} ${res.error || ''} ${res.body}`);
  }
  check(res, { 'status is 201 or 409': (r) => r.status === 201 || r.status === 409 });
}

export function teardown(data) {
  const seatMap = http.get(url(`/v1/showtimes/${data.showtimeId}/seats`));
  const seat = seatMap.json('seats').find((s) => s.id === data.seatId);
  check(seat, { 'the seat is held': (s) => s.status === 'held' });

  const active = activeBookings(data.tokens, data.showtimeId);
  const found = active.reduce((n, ids) => n + ids.length, 0);
  winners.add(found);
  console.log(`bookings of the race users for the showtime after the race: ${found}`);

  if (__ENV.KEEP === '1') {
    console.log('KEEP=1: the winning booking stays; its hold expires on its own');
    return;
  }
  cancel(data.tokens, active);
}
