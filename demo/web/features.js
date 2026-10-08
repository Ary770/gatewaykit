// Every scenario calls the real gateway; summaries retain the actual responses.
function requireDemoResult(condition, message) {
  if (!condition) throw new Error(message);
}

async function demonstrateTransforms() {
  const input = {userId: 7, userName: 'Ary'};
  const response = await request('/transform/echo', {
    method: 'POST',
    headers: {'Content-Type': 'application/json', 'X-Debug': 'remove-me'},
    body: JSON.stringify(input)
  });
  const received = response.body.data;
  requireDemoResult(response.status === 200 && received, 'Transformation request failed');
  const transformed = JSON.parse(received.body);
  requireDemoResult(transformed.user.id === 7 && !received.headers['X-Debug'] &&
    received.headers['X-Gateway'][0] === 'gatewaykit' && response.headers['x-served-by'] === 'gatewaykit',
    'Transformation result did not match the configured rules');
  return {status: 'PASS', sent: input, backendReceived: transformed,
    explanation: 'Gateway reshaped JSON, removed X-Debug, added X-Gateway, and wrapped the response.', response};
}

async function demonstrateRetry() {
  const withoutRetry = await request('/retry-baseline/echo');
  const withRetry = await request('/retry/echo');
  requireDemoResult(withoutRetry.status === 503 && withRetry.status === 200 &&
    withRetry.body.upstream === '127.0.0.1:3006', 'Expected failing backend followed by retry recovery');
  return {status: 'PASS', explanation: 'Backend 3005 returns 503. The retry route waits 100ms and tries backend 3006.',
    withoutRetry, withRetry};
}

async function demonstrateHealthChecks() {
  const unavailable = await request('/offline');
  const responses = [];
  for (let index = 0; index < 8; index++) responses.push(await request('/healthy/echo'));
  requireDemoResult(unavailable.status === 502 && responses.every(response =>
    response.status === 200 && response.body.upstream === '127.0.0.1:3006'),
    'Expected all requests to use the healthy backend; wait a moment for the first probe');
  return {status: 'PASS', explanation: 'Port 39991 has no server. Active probes exclude it; all eight requests use 3006.',
    configuredBackends: ['127.0.0.1:39991 (offline)', '127.0.0.1:3006 (healthy)'], unavailable, responses};
}

async function demonstrateCircuitBreaker() {
  // A previous click may have left the circuit open. Wait, then establish recovery.
  await new Promise(resolve => setTimeout(resolve, 3200));
  const initial = await request('/breaker/echo');
  requireDemoResult(initial.status === 200, 'Circuit did not recover before the scenario');
  const firstFailure = await request('/breaker/echo?status=503');
  const secondFailure = await request('/breaker/echo?status=503');
  const blocked = await request('/breaker/echo');
  await new Promise(resolve => setTimeout(resolve, 3200));
  const recovered = await request('/breaker/echo');
  requireDemoResult(firstFailure.status === 503 && secondFailure.status === 503 &&
    blocked.status === 503 && blocked.body.error === 'service_unavailable' &&
    !blocked.headers['x-mock-upstream'] && recovered.status === 200,
    'Circuit did not follow failure, rejection, and recovery sequence');
  return {status: 'PASS', explanation: 'Two backend failures open the circuit. A healthy request is blocked locally. After 3s, a successful probe closes it.',
    firstFailure, secondFailure, blocked, recovered};
}

const featureCases = {
  transform: demonstrateTransforms,
  retry: demonstrateRetry,
  activeHealth: demonstrateHealthChecks,
  breaker: demonstrateCircuitBreaker
};
