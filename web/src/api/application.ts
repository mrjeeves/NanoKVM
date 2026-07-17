import { http } from '@/lib/http.ts';

// get current firmware version + the latest on our release channel
export function getVersion() {
  return http.get('/api/application/version');
}

// update the firmware to our channel's latest release
export function update() {
  return http.request({
    method: 'post',
    url: '/api/application/update',
    timeout: 15 * 60 * 1000
  });
}
