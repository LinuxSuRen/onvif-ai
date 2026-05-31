/**
 * Type declarations for jmuxer — H.264/AAC muxer for Media Source Extensions.
 */
declare module 'jmuxer' {
  interface JMuxerOptions {
    node: string | HTMLElement;
    mode?: 'video' | 'audio' | 'both';
    videoCodec?: 'H264' | 'H265';
    flushingTime?: number;
    maxDelay?: number;
    clearBuffer?: boolean;
    fps?: number;
    readFpsFromTrack?: boolean;
    debug?: boolean;
    onReady?: () => void;
    onData?: () => void;
    onError?: (error: unknown) => void;
    onUnsupportedCodec?: () => void;
    onMissingVideoFrames?: () => void;
    onMissingAudioFrames?: () => void;
    onKeyframePosition?: (time: number) => void;
  }

  interface JMuxerFeedData {
    video?: Uint8Array;
    audio?: Uint8Array;
    duration?: number;
  }

  export default class JMuxer {
    constructor(options: JMuxerOptions);
    feed(data: JMuxerFeedData): void;
    destroy(): void;
    createStream(): { write(data: JMuxerFeedData): void };
    static isSupported(codec: string): boolean;
  }
}
