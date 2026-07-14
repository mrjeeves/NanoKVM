import { http } from '@/lib/http.ts';

// get mesh bridge status
export function getMeshStatus() {
  return http.get('/api/mesh/status');
}

// rotate the claim code (mints a fresh one; can only invalidate the old
// code, never enable claiming — enabling lives in server.yaml)
export function rotateClaimCode() {
  return http.post('/api/mesh/claim/code/rotate');
}

// get CEC hand-raise status (whether a hand is up + the support number)
export function getHelpStatus() {
  return http.get('/api/mesh/help');
}

// toggle the CEC hand raise (raise if down, lower if up); returns the new state
export function toggleHand() {
  return http.post('/api/mesh/help/toggle');
}
