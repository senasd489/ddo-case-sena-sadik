// EnerjiHat yük testi — make load (k6). Karışık trafik:
//  %40 personel okuma (abone tüketimi, kesinti listesi, etki analizi)
//  %30 abone portalı (kendi günlük + 15 dk tüketimi, fatura önizleme)
//  %30 sayaç veri toplama (500 okumalık partiler, X-Ingest-Token)
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const API = __ENV.API_URL || 'http://localhost:8080';
const INGEST = __ENV.INGEST_TOKEN;
const SUBS = Number(__ENV.SUBSCRIBERS || 5000);
const ingestLatency = new Trend('ingest_batch_duration', true);

export const options = {
  scenarios: {
    staff:  { executor: 'constant-vus', vus: 8, duration: __ENV.DURATION || '60s', exec: 'staff' },
    portal: { executor: 'constant-vus', vus: 6, duration: __ENV.DURATION || '60s', exec: 'portal' },
    ingest: { executor: 'constant-vus', vus: 4, duration: __ENV.DURATION || '60s', exec: 'ingest' },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{scenario:staff}': ['p(95)<500'],
    'http_req_duration{scenario:portal}': ['p(95)<500'],
    ingest_batch_duration: ['p(95)<2000'],
  },
};

function login(email) {
  const r = http.post(`${API}/api/v1/auth/login`, JSON.stringify({ email, password: 'Demo1234!' }), { headers: { 'Content-Type': 'application/json' } });
  return r.json('access_token');
}

export function setup() {
  return { op: login('operator@enerjihat.local'), sub: login('abone@enerjihat.local') };
}

const auth = (t) => ({ headers: { Authorization: `Bearer ${t}` } });
const day = (k) => new Date(Date.now() - k * 86400e3).toISOString().slice(0, 10);

export function staff(d) {
  const id = 1 + Math.floor(Math.random() * SUBS);
  const x = Math.random();
  let r;
  if (x < 0.5) r = http.get(`${API}/api/v1/subscribers/${id}/consumption?granularity=day&from=${day(30)}&to=${day(0)}`, { ...auth(d.op), tags: { name: 'consumption_day' } });
  else if (x < 0.8) r = http.get(`${API}/api/v1/outages?open=true&limit=50`, { ...auth(d.op), tags: { name: 'outages_open' } });
  else r = http.get(`${API}/api/v1/network/nodes/${1 + Math.floor(Math.random() * 100)}/impact`, { ...auth(d.op), tags: { name: 'impact' } });
  check(r, { '200': (r) => r.status === 200 });
}

export function portal(d) {
  const x = Math.random();
  let r;
  if (x < 0.5) r = http.get(`${API}/api/v1/subscribers/1/consumption?granularity=day&from=${day(30)}&to=${day(0)}`, { ...auth(d.sub), tags: { name: 'portal_day' } });
  else if (x < 0.8) r = http.get(`${API}/api/v1/subscribers/1/consumption?granularity=15m&from=${day(2)}&to=${day(1)}`, { ...auth(d.sub), tags: { name: 'portal_15m' } });
  else r = http.get(`${API}/api/v1/subscribers/1/invoice-preview?period=${day(0).slice(0, 7)}`, { ...auth(d.sub), tags: { name: 'portal_preview' } });
  check(r, { '200': (r) => r.status === 200 });
}

// Gerçekçi head-end davranışı: her VU kendi 500 sayacından sorumlu ve her
// iterasyonda bir sonraki 15 dk slotunu gönderir (Temmuz 2026 — demo
// verisine dokunmaz). Böylece seri kesintisizdir; boşluk/düzeltme üretmez.
export function ingest() {
  const t = new Date(Date.UTC(2026, 6, 1) + __ITER * 900e3).toISOString();
  const readings = [];
  for (let i = 0; i < 500; i++) {
    const meter = 1 + ((__VU - 1) * 500 + i) % SUBS;
    readings.push({ meter_id: meter, read_at: t, active_index_wh: 1e7 + __ITER * (100 + i), reactive_index_varh: 0 });
  }
  const r = http.post(`${API}/api/v1/readings/batch`, JSON.stringify({ readings }), {
    headers: { 'Content-Type': 'application/json', 'X-Ingest-Token': INGEST }, tags: { name: 'readings_batch' } });
  ingestLatency.add(r.timings.duration);
  check(r, { '202': (r) => r.status === 202 });
}
