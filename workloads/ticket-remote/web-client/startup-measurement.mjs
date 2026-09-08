// Navigation-relative durations use only the browser's monotonic clock. Repaints
// of the same frame (including SDR/HDR switching) never inflate the frame count.
export class StartupMeasurement {
  constructor(publish) {
    this.publish = publish;
    this.firstMillis = null;
    this.tenMillis = null;
    this.pictures = new Set();
    this.openings = 0;
    this.kind = 'unknown';
    this.logged = false;
  }
  opening(kind) { if (this.kind === 'unknown') this.kind = kind; }
  connect() { this.openings++; }
  presented(metadata, millis) {
    if (this.pictures.size >= 10) return;
    const key = `${metadata.epoch}:${metadata.sequence}`;
    if (this.pictures.has(key)) return;
    this.pictures.add(key);
    if (this.firstMillis === null) this.firstMillis = Math.round(millis);
    if (this.pictures.size === 10) { this.tenMillis = Math.round(millis); this.flush(); }
  }
  snapshot() {
    return { firstPresentedMillis:this.firstMillis, tenPresentedMillis:this.tenMillis,
      presentedFrames:this.pictures.size, reconnects:Math.max(0,this.openings-1), openingClass:this.kind };
  }
  flush() {
    if (this.logged || this.firstMillis === null) return;
    this.logged = true;
    this.publish(this.snapshot());
  }
}
