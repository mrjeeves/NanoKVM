import { useEffect, useState } from 'react';
import { useAtomValue } from 'jotai';

import { videoModeAtom } from '@/jotai/screen.ts';

import { H264Direct } from './h264-direct.tsx';
import { H264Webrtc } from './h264-webrtc.tsx';
import { Mjpeg } from './mjpeg.tsx';

export const Screen = () => {
  const videoMode = useAtomValue(videoModeAtom);

  // Ephemeral, per-attempt fallback flag: when the default WebRTC path fails to
  // bring up media we auto-drop to MJPEG WITHOUT touching the user's videoMode
  // preference. Switching modes re-arms WebRTC (see the reset below).
  const [webrtcFailed, setWebrtcFailed] = useState(false);

  useEffect(() => {
    setWebrtcFailed(false);
  }, [videoMode]);

  if (videoMode === 'mjpeg') {
    return <Mjpeg />;
  }

  if (videoMode === 'direct') {
    return <H264Direct />;
  }

  // Default WebRTC path: attempt H.264-over-WebRTC, dropping to MJPEG if the
  // media never connects.
  if (webrtcFailed) {
    return <Mjpeg />;
  }

  return <H264Webrtc onFailure={() => setWebrtcFailed(true)} />;
};
