// Only the phone recognizes a ViVi result. The browser identifies its picture.
export function exactResultMatches(request, epoch, sequence) {
  if (!request || request.status !== 'succeeded' || !request.captureRequired) return false;
  const markerEpoch = String(request.resultFrameEpoch || '0');
  const markerSequence = String(request.resultMinFrameSequence || '0');
  return markerEpoch !== '0' && markerSequence !== '0' &&
    String(epoch) === markerEpoch && String(sequence) === markerSequence &&
    request.resultMarkerRevision === `${markerEpoch}:${markerSequence}`;
}
