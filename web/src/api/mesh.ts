import { http } from '@/lib/http.ts';

// get mesh bridge status
export function getMeshStatus() {
  return http.get('/api/mesh/status');
}
