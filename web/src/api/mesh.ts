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

// Read support number, pending requests and the one-request approval window.
export function getHelpStatus() {
  return http.get('/api/mesh/help');
}

// Approve the current request or refresh the five-minute window for one request.
export function armSupportApproval() {
  return http.post('/api/mesh/help/arm');
}
export function decideSupportRequest(technician: string, sessionId: string, approve: boolean) {
  return http.post(`/api/mesh/help/${approve ? 'approve' : 'deny'}`, { technician, sessionId });
}

// reset this device's mesh ownership back to claim mode (forget owner + fleet).
// Local-network only — the server rejects it over the mesh tunnel.
export function unclaimDevice() {
  return http.post('/api/mesh/unclaim');
}
