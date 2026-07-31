import { http } from '@/lib/http.ts';

// get current firmware version + the latest on our release channel
//
// The server caches the channel lookup, because it used to make a network
// round-trip on every call and the unauthenticated GitHub budget (60/hour per
// IP, shared by every device behind it) does not stretch to that. `refresh`
// bypasses the cache: pass it when a person actually pressed "Check for
// Updates", so the button means what it says, and leave it off for the
// incidental read on page load.
export function getVersion(refresh = false) {
  return http.get(`/api/application/version${refresh ? '?refresh=1' : ''}`);
}

// update the firmware to our channel's latest release
export function update() {
  return http.request({
    method: 'post',
    url: '/api/application/update',
    timeout: 15 * 60 * 1000
  });
}
