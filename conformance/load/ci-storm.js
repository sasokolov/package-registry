// k6 profile "CI storm": many concurrent CI jobs resolving the same small
// dependency set through the registry, which is what a real morning looks
// like. The cache is warm, so this measures the registry's serving path
// rather than the upstream.
import http from 'k6/http';
import { check } from 'k6';
import { Rate } from 'k6/metrics';

const BASE = __ENV.REGISTRY_BASE || 'http://lb';
const VUS = Number(__ENV.VUS || 200);
const DURATION = __ENV.DURATION || '30s';

export const cacheHits = new Rate('registry_cache_hits');

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
  scenarios: {
    ci_storm: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
    },
  },
  thresholds: {
    // A warm cache must not fail requests, and served-from-cache is the
    // whole point of the registry.
    http_req_failed: ['rate<0.01'],
    registry_cache_hits: ['rate>0.95'],
  },
};

// One "job" resolves metadata and then downloads the artifacts, like npm ci.
const requests = [
  '/npm/npmjs/left-pad',
  '/npm/npmjs/is-odd',
  '/npm/npmjs/@scope%2futil',
  '/npm/npmjs/left-pad/-/left-pad-1.3.0.tgz',
  '/npm/npmjs/is-odd/-/is-odd-3.0.1.tgz',
  '/npm/npmjs/@scope/util/-/util-2.1.0.tgz',
  '/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar',
  '/maven/central/com/example/liba/1.0.0/liba-1.0.0.pom',
  '/maven/central/com/example/liba/1.0.0/liba-1.0.0.jar.sha1',
  '/maven/central/com/example/libb/1.0.0/libb-1.0.0.jar',
];

// handleSummary writes a machine-readable summary next to the human one, so
// run-load.sh can turn it into the baseline table in docs/perf.md.
export function handleSummary(data) {
  return {
    stdout: '\n',
    '/out/summary.json': JSON.stringify(data),
  };
}

export default function () {
  for (const path of requests) {
    const res = http.get(`${BASE}${path}`);
    check(res, { 'status is 200': (r) => r.status === 200 });
    const source = res.headers['X-Registry-Source'];
    cacheHits.add(source === 'cache' || source === 'local');
  }
}
