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
