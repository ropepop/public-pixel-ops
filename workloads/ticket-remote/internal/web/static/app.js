(function () {
  const cfg = window.TICKET_REMOTE_CONFIG || {};
  const pageVersion = cfg.pageVersion || 'ticket-remote-dev';

  if ('scrollRestoration' in history) {
    history.scrollRestoration = 'manual';
  }

  if (document.querySelector('[data-admin="true"]')) {
    startAdmin();
    return;
  }

  const stage = document.querySelector('.stage');
  const canvas = document.getElementById('screen');
  const ctx = canvas.getContext('2d', { alpha: false });
  const emptyState = document.getElementById('emptyState');
  const startStreamButton = document.getElementById('startStream');
  const emptyMessage = document.getElementById('emptyMessage');
  const privacyOverlay = document.getElementById('privacyOverlay');
  const privacyText = document.getElementById('privacyText');
  const connectionState = document.getElementById('connectionState');
  const statusLine = document.getElementById('statusLine');
  const panel = document.getElementById('panel');
  const presence = document.getElementById('presence');
  const claimButton = document.getElementById('claimControl');
  const extendButton = document.getElementById('extendControl');
  const releaseButton = document.getElementById('releaseControl');
  const timer = document.getElementById('timer');

  let ws = null;
  let videoWs = null;
  let reconnectTimer = null;
  let configured = false;
  let streamUnsupported = false;
  let streamSize = { width: 540, height: 1080 };
  let currentState = null;
  let serverClockSkewMs = 0;
  let pointerStart = null;
  let connectedAt = 0;
  let configuredAt = 0;
  let lastFrameAt = 0;
  let lastRestartAt = 0;
  let lastFirstFrameNudgeAt = 0;
  let decoder = null;
  let decoderConfigured = false;
  let needsKeyFrame = true;
  let currentStreamEpoch = 0;
  let lastAcceptedFrameSequence = 0;
  let firstFrameReceived = false;
  let claimPromise = null;
  let inputSeq = 0;
  let inputInFlight = null;
  const inputQueue = [];
  const inputQueueLimit = 20;
  const inputAckTimeoutMs = 1800;
  const inputRetryLimit = 1;
  let lastTouchEndAt = 0;
  let lastTouchEndX = 0;
  let lastTouchEndY = 0;
  const maxTapDurationMs = 450;
  const maxTapTravelPx = 14;
  const streamVerticalPanThresholdPx = 18;
  const streamVerticalPanDominance = 1.25;
  const streamWatchdogMs = 4500;
  const streamStaleRestartMs = 20000;
  const streamStartupGraceMs = 2000;
  const streamStartupHardErrorMs = 12000;
  const FRAME_ENVELOPE_MAGIC = 0x54534632;
  const FRAME_ENVELOPE_HEADER_BYTES = 29;
  const doubleTapSuppressMs = 420;
  const doubleTapSuppressPx = 28;
  const quickClaimMaxX = 0.25;
  const quickClaimMaxY = 0.25;
  const controlCodeButtonMinX = 0.04;
  const controlCodeButtonMaxX = 0.45;
  const controlCodeButtonMinY = 0.10;
  const controlCodeButtonMaxY = 0.18;

  function viewportHeight() {
    return Math.max(1, Math.round((window.visualViewport && window.visualViewport.height) || window.innerHeight || document.documentElement.clientHeight || 1));
  }

  function updateViewportVars() {
    document.documentElement.style.setProperty('--ticket-stage-height', `${viewportHeight()}px`);
  }

  function updateDetailsReveal() {
    const revealed = window.scrollY >= Math.max(1, viewportHeight() * 0.82);
    document.body.classList.toggle('details-visible', revealed);
    if (panel) panel.setAttribute('aria-hidden', revealed ? 'false' : 'true');
  }

  function keepFirstScreenPinned(force) {
    if (force) {
      document.body.classList.remove('details-visible');
      if (panel) panel.setAttribute('aria-hidden', 'true');
    }
    if (force || !document.body.classList.contains('details-visible')) {
      window.scrollTo({ top: 0, left: 0, behavior: 'auto' });
      updateDetailsReveal();
    }
  }

  function scheduleFirstScreenPin(force) {
    keepFirstScreenPinned(force);
    requestAnimationFrame(() => keepFirstScreenPinned(force));
    setTimeout(() => keepFirstScreenPinned(force), 60);
    setTimeout(() => keepFirstScreenPinned(force), 300);
  }

  function checkServerVersion(payload) {
    const serverVersion = payload && payload.serverVersion;
    if (!serverVersion || serverVersion === pageVersion) return true;
    if (!String(serverVersion).startsWith('ticket-remote-')) return true;
    const next = new URL(location.href);
    next.searchParams.set('v', serverVersion);
    location.replace(next.toString());
    return false;
  }

  async function refreshHealth() {
    try {
      const response = await fetch('/api/v1/health', { cache: 'no-store' });
      const health = await response.json();
      checkServerVersion(health);
      return health;
    } catch (error) {
      clientLog('health_check_failed', error && error.message);
      return null;
    }
  }

  document.body.dataset.videoPath = 'https-h264';

  function socketURL() {
    return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/v1/session';
  }

  function streamURL() {
    return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/v1/stream';
  }

  function setConnected(text) {
    connectionState.textContent = text;
  }

  const publicMessageTranslations = new Map([
    ['Phone stream reconnecting', 'Tālruņa straume savienojas no jauna'],
    ['Ticket server is starting', 'Biļetes serveris startējas'],
    ['Ticket server is stopped', 'Biļetes serveris ir apturēts'],
    ['Ticket session is active through root capture', 'Biļetes sesija darbojas ar root ekrāna tveršanu'],
    ['Root capture is idle', 'Root ekrāna tveršana ir gaidstāvē'],
    ['Root shell is unavailable', 'Root komandrinda nav pieejama'],
    ['Root screenrecord capture is available', 'Root ekrāna tveršana ir pieejama'],
    ['Root capture is starting', 'Root ekrāna tveršana startējas'],
    ['Root capture is active', 'Root ekrāna tveršana ir aktīva'],
    ['Root capture is unavailable', 'Root ekrāna tveršana nav pieejama'],
    ['ViVi is not installed from a local Pixel app store yet', 'ViVi vēl nav instalēta no vietējā Pixel lietotņu veikala'],
    ['ViVi launch intent is unavailable', 'ViVi palaišana nav pieejama'],
    ['No visible frame has been sent yet', 'Vēl nav nosūtīts neviens redzams kadrs'],
    ['Unavailable', 'Nav pieejams'],
    ['Connection failed', 'Savienojums neizdevās'],
    ['Video connection failed', 'Video savienojums neizdevās'],
    ['control_claimed', 'Kontrole jau ir pārņemta'],
    ['no_control', 'Nav aktīvas kontroles sesijas'],
    ['not_controller', 'Šo kontroles sesiju pārvalda cits lietotājs'],
    ['already_extended', 'Sesija jau ir pagarināta']
  ]);

  function localizePublicMessage(value) {
    if (!value) return '';
    const text = String(value);
    const exact = publicMessageTranslations.get(text);
    if (exact) return exact;
    for (const [prefix, translation] of [
      ['Ticket server is listening on ', 'Biļetes serveris klausās uz '],
      ['Ticket server failed to start: ', 'Biļetes serveri neizdevās palaist: '],
      ['Ticket session stopped: ', 'Biļetes sesija apturēta: '],
      ['Root capture stopped: ', 'Root ekrāna tveršana apturēta: '],
      ['Root capture restarting: ', 'Root ekrāna tveršana restartējas: '],
      ['Root capture exited with code ', 'Root ekrāna tveršana aizvērās ar kodu '],
      ['Root capture stream closed during restart', 'Root ekrāna tveršanas straume aizvērās restartēšanas laikā'],
      ['Root capture failed: ', 'Root ekrāna tveršana neizdevās: ']
    ]) {
      if (text.startsWith(prefix)) return translation + text.slice(prefix.length);
    }
    return text;
  }

  function setStatus(text) {
    statusLine.textContent = localizePublicMessage(text);
  }

  function clientLog(event, detail) {
    let safeDetail = '';
    if (detail != null && typeof detail === 'object') {
      try {
        safeDetail = JSON.stringify(detail);
      } catch (_) {
        safeDetail = String(detail);
      }
    } else {
      safeDetail = String(detail || '');
    }
    fetch('/api/v1/client-log', {
      method: 'POST',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        event,
        pageVersion,
        detail: safeDetail.slice(0, 500),
        webCodecs: 'VideoDecoder' in window,
        userAgent: navigator.userAgent
      })
    }).catch(() => {});
  }

  function showEmpty(message, showStart) {
    emptyMessage.textContent = localizePublicMessage(message);
    startStreamButton.hidden = true;
    emptyState.hidden = false;
    document.body.dataset.streamReady = 'false';
    keepFirstScreenPinned();
  }

  function hideEmpty() {
    emptyState.hidden = true;
    document.body.dataset.streamReady = 'true';
    keepFirstScreenPinned();
  }

  function showUnsupported(message) {
    streamUnsupported = true;
    configured = false;
    showEmpty(message, false);
    setStatus(message);
    clientLog('h264_unsupported', message);
  }

  function resizeCanvasBox() {
    updateViewportVars();
    const maxWidth = Math.max(1, stage.clientWidth);
    const maxHeight = Math.max(1, stage.clientHeight);
    const scale = Math.min(maxWidth / streamSize.width, maxHeight / streamSize.height);
    stage.style.setProperty('--stream-width', `${Math.max(1, Math.floor(streamSize.width * scale))}px`);
    stage.style.setProperty('--stream-height', `${Math.max(1, Math.floor(streamSize.height * scale))}px`);
  }

  function connect() {
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
    clearTimeout(reconnectTimer);
    keepFirstScreenPinned();
    setConnected('Savienojas');
    connectedAt = performance.now();
    ws = new WebSocket(socketURL());
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => {
      setConnected('Savienots');
      if (!streamUnsupported) {
        showEmpty(configured ? 'Gaida tiešraides kadru...' : 'Gaida biļetes straumi...', false);
      }
      send({ type: 'heartbeat' });
      connectDirectVideo();
      processInputQueue();
    };
    ws.onmessage = handleMessage;
    ws.onclose = () => {
      setConnected('Savienojas no jauna');
      configured = false;
      streamUnsupported = false;
      keepFirstScreenPinned();
      closeDirectVideo();
      showEmpty('Atjauno straumi...', false);
      reconnectTimer = setTimeout(connect, 1000);
    };
    ws.onerror = () => {
      setConnected('Savienojuma kļūme');
      if (!streamUnsupported) {
        showEmpty('Atjauno straumi...', false);
      }
      clientLog('websocket_error', 'socket error');
    };
    connectDirectVideo();
  }

  function resetStreamState() {
    configured = false;
    configuredAt = 0;
    lastFrameAt = 0;
    lastFirstFrameNudgeAt = 0;
    firstFrameReceived = false;
    needsKeyFrame = true;
    currentStreamEpoch = 0;
    lastAcceptedFrameSequence = 0;
    closeDecoder();
  }

  function restartStream(reason) {
    if (streamUnsupported) return;
    const now = performance.now();
    if (now - lastRestartAt < 5000) return;
    lastRestartAt = now;
    clientLog('video_stream_restart', reason);
    closeDirectVideo();
    resetStreamState();
    showEmpty('Atjauno straumi...', false);
    setTimeout(connectDirectVideo, 250);
  }

  function closeDirectVideo() {
    closeDecoder();
    if (videoWs) {
      try { videoWs.close(); } catch (_) {}
      videoWs = null;
    }
  }

  function connectDirectVideo() {
    if (videoWs && (videoWs.readyState === WebSocket.OPEN || videoWs.readyState === WebSocket.CONNECTING)) return;
    closeDirectVideo();
    document.body.dataset.videoPath = 'https-h264';
    videoWs = new WebSocket(streamURL());
    videoWs.binaryType = 'arraybuffer';
    videoWs.onopen = () => {
      showEmpty('Saņem video konfigurāciju...', false);
      requestKeyframe('video_socket_open');
    };
    videoWs.onmessage = handleVideoSocketMessage;
    videoWs.onclose = () => {
      if (ws && ws.readyState !== WebSocket.CLOSED && ws.readyState !== WebSocket.CLOSING) {
        setTimeout(connectDirectVideo, 1000);
      }
    };
    videoWs.onerror = () => {
      clientLog('direct_video_websocket_error', 'socket error');
    };
  }

  function sendVideoSignal(value) {
    if (videoWs && videoWs.readyState === WebSocket.OPEN) {
      videoWs.send(JSON.stringify(value));
      return true;
    }
    return false;
  }

  function sendVideoClientLog(event, detail) {
    let safeDetail = '';
    if (detail != null && typeof detail === 'object') {
      try {
        safeDetail = JSON.stringify(detail);
      } catch (_) {
        safeDetail = String(detail);
      }
    } else {
      safeDetail = String(detail || '');
    }
    sendVideoSignal({ type: 'client_log', event, detail: safeDetail.slice(0, 500) });
    clientLog(event, detail);
  }

  function requestKeyframe(reason) {
    if (!sendVideoSignal({ type: 'keyframe', reason })) {
      send({ type: 'keyframe', reason });
    }
  }

  function closeDecoder() {
    if (decoder) {
      try { decoder.close(); } catch (_) {}
      decoder = null;
    }
    decoderConfigured = false;
  }

  function publishStreamDebug() {
    window.ticketStreamDebug = {
      pageVersion,
      configured,
      streamReady: document.body.dataset.streamReady,
      transport: 'https-websocket-h264',
      codec: decoderConfigured ? 'h264' : '',
      currentStreamEpoch,
      lastAcceptedFrameSequence,
      needsKeyFrame,
      firstFrameReceived
    };
  }

  function readUint64(view, offset) {
    return view.getUint32(offset) * 4294967296 + view.getUint32(offset + 4);
  }

  function parseFrameEnvelope(raw) {
    const data = new Uint8Array(raw);
    const view = new DataView(raw);
    if (data.byteLength >= FRAME_ENVELOPE_HEADER_BYTES && view.getUint32(0) === FRAME_ENVELOPE_MAGIC) {
      const flags = view.getUint8(4);
      return {
        version: 'tsf2',
        kind: (flags & 1) === 1 ? 'key' : 'delta',
        epoch: readUint64(view, 5),
        sequence: readUint64(view, 13),
        timestamp: readUint64(view, 21),
        data: data.slice(FRAME_ENVELOPE_HEADER_BYTES)
      };
    }
    sendVideoClientLog('invalid_tsf2_frame', `bytes=${data.byteLength}`);
    showUnsupported('Video stream sent an invalid frame. Refresh and try again.');
    return null;
  }

  function acceptFreshFrame(frame) {
    if (!frame) return false;
    if (currentStreamEpoch && frame.epoch && frame.epoch !== currentStreamEpoch) {
      return false;
    }
    if (frame.sequence && frame.sequence <= lastAcceptedFrameSequence) {
      return false;
    }
    if (needsKeyFrame && frame.kind !== 'key') {
      return false;
    }
    if (frame.kind === 'key') needsKeyFrame = false;
    if (frame.sequence) lastAcceptedFrameSequence = frame.sequence;
    return true;
  }

  async function handleVideoSocketMessage(event) {
    if (typeof event.data === 'string') {
      let msg;
      try { msg = JSON.parse(event.data); } catch (_) { return; }
      if (!checkServerVersion(msg)) return;
      if (msg.type === 'config') {
        await configureDecoder(msg);
      } else if (msg.type === 'state' || msg.type === 'phone' || msg.type === 'health') {
        handleMessage({ data: event.data });
      }
      return;
    }
    if (!configured || !decoder) return;
    const frame = parseFrameEnvelope(event.data);
    if (!acceptFreshFrame(frame)) return;
    try {
      decoder.decode(new EncodedVideoChunk({ type: frame.kind, timestamp: frame.timestamp, data: frame.data }));
    } catch (error) {
      sendVideoClientLog('decoder_decode_failed', error && error.message || 'decode failed');
      needsKeyFrame = true;
      requestKeyframe('decoder_decode_failed');
    }
  }

  async function configureDecoder(config) {
    if (!('VideoDecoder' in window) || !('EncodedVideoChunk' in window)) {
      showUnsupported('Šī pārlūkprogramma neatbalsta H.264 video dekodēšanu šajā lapā.');
      return;
    }
    const codec = String(config.codec || '');
    const transport = String(config.transport || '');
    const h264 = codec.startsWith('avc1') || transport === 'h264-annexb' || transport === 'ffmpeg-h264-annexb';
    if (!h264) {
      showUnsupported('Šī straume nav H.264 video.');
      return;
    }
    const width = Number(config.width || 0);
    const height = Number(config.height || 0);
    if (!width || !height) {
      showUnsupported('Video konfigurācija nav pilnīga.');
      return;
    }
    const decoderConfig = { codec, codedWidth: width, codedHeight: height, avc: { format: 'annexb' } };
    let supported = false;
    try {
      const result = await VideoDecoder.isConfigSupported(decoderConfig);
      supported = Boolean(result && result.supported);
    } catch (error) {
      supported = false;
    }
    if (!supported) {
      showUnsupported('Šī pārlūkprogramma nevar atvērt H.264 biļetes video.');
      return;
    }
    closeDecoder();
    canvas.width = width;
    canvas.height = height;
    ctx.imageSmoothingEnabled = false;
    streamSize = { width, height };
    currentStreamEpoch = Number(config.streamEpoch || 0);
    lastAcceptedFrameSequence = 0;
    needsKeyFrame = true;
    configured = true;
    configuredAt = performance.now();
    firstFrameReceived = false;
    resizeCanvasBox();
    decoder = new VideoDecoder({
      output: (frame) => {
        try {
          ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
          lastFrameAt = performance.now();
          firstFrameReceived = true;
          hideEmpty();
          publishStreamDebug();
        } finally {
          frame.close();
        }
      },
      error: (error) => {
        sendVideoClientLog('decoder_error', error && error.message || 'decoder error');
        needsKeyFrame = true;
        restartStream('decoder_error');
      }
    });
    decoder.configure(decoderConfig);
    decoderConfigured = true;
    publishStreamDebug();
    showEmpty('Gaida pirmo video kadru...', false);
    requestKeyframe('config_received');
    keepFirstScreenPinned();
  }

  function send(value) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(value));
      return true;
    }
    return false;
  }

  function nextInputId() {
    inputSeq += 1;
    return `${cfg.sessionId || 'ticket'}-${Date.now().toString(36)}-${inputSeq}`;
  }

  function inputQueueSize() {
    return inputQueue.length + (inputInFlight ? 1 : 0);
  }

  function queueInput(value) {
    if (inputQueueSize() >= inputQueueLimit) {
      setStatus('Pieskārienu rinda ir pilna. Uzgaidi mirkli un mēģini vēlreiz.');
      return false;
    }
    inputQueue.push({
      ...value,
      inputId: value.inputId || nextInputId(),
      retryCount: value.retryCount || 0
    });
    processInputQueue();
    return true;
  }

  function queueTap(screenPoint, options) {
    const value = {
      type: 'tap',
      x: screenPoint.x,
      y: screenPoint.y
    };
    if (options && options.snapTarget) {
      value.snapTarget = options.snapTarget;
    }
    return queueInput(value);
  }

  function processInputQueue() {
    if (inputInFlight || inputQueue.length === 0) return;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      setStatus('Gaida savienojumu, lai nosūtītu pieskārienu.');
      return;
    }
    inputInFlight = inputQueue.shift();
    if (!send(inputInFlight)) {
      inputQueue.unshift(inputInFlight);
      inputInFlight = null;
      setStatus('Gaida savienojumu, lai nosūtītu pieskārienu.');
      return;
    }
    inputInFlight.timeout = setTimeout(() => retryOrDropInput(inputInFlight.inputId), inputAckTimeoutMs);
  }

  function retryOrDropInput(inputId) {
    if (!inputInFlight || inputInFlight.inputId !== inputId) return;
    const timedOut = inputInFlight;
    inputInFlight = null;
    if (timedOut.retryCount < inputRetryLimit) {
      timedOut.retryCount += 1;
      delete timedOut.timeout;
      inputQueue.unshift(timedOut);
      setStatus('Pieskāriens netika apstiprināts, mēģina vēlreiz.');
      processInputQueue();
      return;
    }
    setStatus('Pieskāriens netika apstiprināts.');
    processInputQueue();
  }

  function finishInput(inputId, accepted, reason) {
    if (!inputInFlight || inputInFlight.inputId !== inputId) return;
    if (inputInFlight.timeout) {
      clearTimeout(inputInFlight.timeout);
    }
    inputInFlight = null;
    if (!accepted) {
      setStatus(reason === 'not_active_controller'
        ? 'Ievade netiek pieņemta, kamēr nav pārņemts kontroles koda režīms.'
        : 'Pieskāriens netika pieņemts.');
    } else if (inputQueue.length > 0) {
      setStatus(`Nosūta pieskārienus: ${inputQueue.length} gaida.`);
    }
    processInputQueue();
  }

  function handleInputMessage(msg) {
    if (msg.type === 'input_result') {
      finishInput(String(msg.inputId || ''), msg.accepted !== false, msg.reason || '');
      return true;
    }
    if (msg.type === 'input' && msg.accepted === false) {
      finishInput(String(msg.inputId || ''), false, msg.reason || '');
      if (!msg.inputId) {
        setStatus('Ievade netiek pieņemta, kamēr nav pārņemts kontroles koda režīms.');
      }
      return true;
    }
    return msg.type === 'input';
  }

  async function handleMessage(event) {
    if (typeof event.data === 'string') {
      let msg;
      try { msg = JSON.parse(event.data); } catch (_) { return; }
      if (msg.type === 'config') {
        configureStreamInfo(msg);
      } else if (msg.type === 'state') {
        currentState = msg.state;
        rememberServerClock(currentState);
        renderState();
      } else if (msg.type === 'health') {
        if (msg.data && msg.data.message) {
          setStatus(msg.data.message);
          if (msg.data.streamActive === false && !streamUnsupported) {
            showEmpty(`${localizePublicMessage(msg.data.message)} Restartē...`, false);
          }
        }
      } else if (msg.type === 'phone') {
        setStatus(msg.message || '');
      } else if (handleInputMessage(msg)) {
        return;
      }
      return;
    }
    clientLog('unexpected_binary_frame', 'binary frame arrived on control socket');
  }

  function configureStreamInfo(config) {
    if (config.width && config.height && !configured) {
      canvas.width = config.width;
      canvas.height = config.height;
      streamSize = { width: config.width, height: config.height };
      resizeCanvasBox();
    }
    if (config.type === 'config' && videoWs && videoWs.readyState === WebSocket.OPEN) {
      configureDecoder(config).catch((error) => sendVideoClientLog('decoder_config_failed', error && error.message || 'config failed'));
    }
  }

  function renderState() {
    const state = currentState;
    if (!state) return;
    rememberServerClock(state);
    const control = currentControl(state);
    const selfControl = control && control.sessionId === cfg.sessionId && control.email === cfg.email;
    const otherControl = control && !selfControl;

    claimButton.hidden = Boolean(control);
    extendButton.hidden = !selfControl || control.extended;
    releaseButton.hidden = !selfControl;

    if (control) {
      const remaining = Math.max(0, Math.ceil((control.remainingMs || 0) / 1000));
      timer.hidden = false;
      timer.textContent = `${remaining}s`;
      timer.classList.toggle('urgent', remaining <= 10);
      setStatus(selfControl ? 'Tev ir privāta tālruņa kontrole.' : `${control.email} ir privāta tālruņa kontrole.`);
    } else {
      timer.hidden = true;
      timer.classList.remove('urgent');
      setStatus('Vispārīga skatīšanās');
    }

    if (otherControl) {
      privacyOverlay.hidden = false;
      privacyText.textContent = `${control.email} izmanto privātu kontroles koda sesiju vēl ${timer.textContent}. Tu paliec pieslēgts un automātiski atgriezīsies kopīgajā skatā.`;
    } else {
      privacyOverlay.hidden = true;
    }

    renderPresence(state.viewers || []);
  }

  function rememberServerClock(state) {
    const parsed = Date.parse(state && state.serverTime);
    if (Number.isFinite(parsed)) {
      serverClockSkewMs = parsed - Date.now();
    }
  }

  function serverNow() {
    return Date.now() + serverClockSkewMs;
  }

  function currentControl(state) {
    const control = state && state.activeControl;
    if (!control) return null;
    const expiresAt = Date.parse(control.expiresAt);
    if (!Number.isFinite(expiresAt)) return control;
    const remainingMs = Math.max(0, expiresAt - serverNow());
    if (remainingMs <= 0) return null;
    return { ...control, remainingMs };
  }

  function renderPresence(viewers) {
    presence.textContent = '';
    const title = document.createElement('div');
    title.textContent = `${viewers.length} lapā`;
    presence.appendChild(title);
    viewers.forEach((viewer) => {
      const row = document.createElement('div');
      row.className = 'presence-item';
      const email = document.createElement('span');
      email.textContent = viewer.email;
      const mark = document.createElement('span');
      mark.textContent = viewer.sessionId === cfg.sessionId ? 'tu' : 'skatās';
      row.append(email, mark);
      presence.appendChild(row);
    });
  }

  function isPrivacyCovered() {
    const control = currentControl(currentState);
    return Boolean(control && (control.sessionId !== cfg.sessionId || control.email !== cfg.email));
  }

  async function postJSON(path, body) {
    const response = await fetch(path, {
      method: 'POST',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : '{}'
    });
    const text = await response.text();
    let payload = {};
    try {
      payload = text ? JSON.parse(text) : {};
    } catch (_) {
      payload = { ok: false, message: text || 'Pieprasījums neizdevās' };
    }
    if (!response.ok || !payload.ok) {
      throw new Error(payload.message || payload.error || 'Pieprasījums neizdevās');
    }
    currentState = payload.state;
    rememberServerClock(currentState);
    renderState();
    return payload;
  }

  async function ensureControl() {
    const control = currentControl(currentState);
    const selfControl = control && control.sessionId === cfg.sessionId && control.email === cfg.email;
    if (selfControl) return;
    if (!claimPromise) {
      claimPromise = postJSON('/api/v1/control/claim').finally(() => {
        claimPromise = null;
      });
    }
    await claimPromise;
  }

  async function claimControl(options) {
    await ensureControl();
    if (options && options.tap) {
      queueTap(options.tap, { snapTarget: options.snapTarget });
    }
  }

  claimButton.addEventListener('click', () => claimControl().catch((error) => setStatus(error.message)));
  extendButton.addEventListener('click', () => postJSON('/api/v1/control/extend').catch((error) => setStatus(error.message)));
  releaseButton.addEventListener('click', () => postJSON('/api/v1/control/release').catch((error) => setStatus(error.message)));
  startStreamButton.addEventListener('click', () => restartStream('manual_start'));

  function point(event) {
    const rect = canvas.getBoundingClientRect();
    const width = canvas.width;
    const height = canvas.height;
    return {
      x: Math.round(((event.clientX - rect.left) / rect.width) * width),
      y: Math.round(((event.clientY - rect.top) / rect.height) * height)
    };
  }

  function firstClaimCandidateZone(screenPoint) {
    const width = canvas.width || streamSize.width || 1;
    const height = canvas.height || streamSize.height || 1;
    const relativeX = screenPoint.x / width;
    const relativeY = screenPoint.y / height;
    if (relativeX >= 0 && relativeX <= quickClaimMaxX && relativeY >= 0 && relativeY <= quickClaimMaxY) {
      return 'top_left_quarter';
    }
    if (
      relativeX >= controlCodeButtonMinX &&
      relativeX <= controlCodeButtonMaxX &&
      relativeY >= controlCodeButtonMinY &&
      relativeY <= controlCodeButtonMaxY
    ) {
      return 'control_code_button_geometry';
    }
    return '';
  }

  canvas.addEventListener('pointerdown', (event) => {
    if (!configured || isPrivacyCovered()) return;
    if (event.button != null && event.button !== 0) return;
    const control = currentControl(currentState);
    const start = point(event);
    const selfControl = control && control.sessionId === cfg.sessionId && control.email === cfg.email;
    pointerStart = {
      ...start,
      clientX: event.clientX,
      clientY: event.clientY,
      pointerId: event.pointerId,
      pointerType: event.pointerType || 'mouse',
      selfControl: Boolean(selfControl),
      claimZone: !control ? firstClaimCandidateZone(start) : '',
      at: performance.now()
    };
    if (event.pointerType === 'mouse') {
      event.preventDefault();
      canvas.setPointerCapture(event.pointerId);
    }
  });

  canvas.addEventListener('pointermove', (event) => {
    if (!pointerStart || pointerStart.pointerId !== event.pointerId) return;
    if (pointerStart.pointerType === 'mouse') return;
    const dx = event.clientX - pointerStart.clientX;
    const dy = event.clientY - pointerStart.clientY;
    if (Math.abs(dy) >= streamVerticalPanThresholdPx && Math.abs(dy) > Math.abs(dx) * streamVerticalPanDominance) {
      pointerStart = null;
      clientLog('stream_vertical_scroll', 'allowed');
    }
  });

  canvas.addEventListener('pointerup', (event) => {
    if (!pointerStart || !configured || isPrivacyCovered()) return;
    if (pointerStart.pointerId !== event.pointerId) return;
    const end = point(event);
    const distance = Math.hypot(end.x - pointerStart.x, end.y - pointerStart.y);
    const heldMs = performance.now() - pointerStart.at;
    if (distance < maxTapTravelPx && heldMs <= maxTapDurationMs) {
      event.preventDefault();
      if (pointerStart.selfControl) {
        queueTap(end);
      } else if (pointerStart.claimZone) {
        claimControl({ tap: { x: pointerStart.x, y: pointerStart.y }, snapTarget: 'control_code_button' }).catch((error) => setStatus(error.message));
      } else {
        setStatus('Pirms pieskaries tālrunim, pārņem kontroles koda režīmu.');
      }
    } else {
      if (event.cancelable) event.preventDefault();
      setStatus('Atbalstīti ir tikai pieskārieni.');
      clientLog('blocked_gesture', distance < maxTapTravelPx ? 'long_press' : 'swipe');
    }
    pointerStart = null;
  });

  canvas.addEventListener('pointercancel', () => {
    pointerStart = null;
  });
  canvas.addEventListener('dblclick', (event) => event.preventDefault());
  function blockStreamGesture(event) {
    if (event.cancelable) {
      event.preventDefault();
    }
  }
  function blockDoubleTapZoom(event) {
    if (event.changedTouches && event.changedTouches.length > 0) {
      const touch = event.changedTouches[0];
      const now = performance.now();
      const nearLastTouch = now - lastTouchEndAt < doubleTapSuppressMs
        && Math.hypot(touch.clientX - lastTouchEndX, touch.clientY - lastTouchEndY) < doubleTapSuppressPx;
      if (nearLastTouch && event.cancelable) {
        event.preventDefault();
      }
      lastTouchEndAt = now;
      lastTouchEndX = touch.clientX;
      lastTouchEndY = touch.clientY;
    }
  }
  canvas.addEventListener('touchend', blockDoubleTapZoom, { passive: false });
  for (const eventName of ['gesturestart', 'gesturechange', 'gestureend']) {
    canvas.addEventListener(eventName, blockStreamGesture, { passive: false });
    document.addEventListener(eventName, blockStreamGesture, { passive: false });
  }
  window.addEventListener('resize', resizeCanvasBox);
  window.addEventListener('scroll', updateDetailsReveal, { passive: true });
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', resizeCanvasBox);
    window.visualViewport.addEventListener('scroll', resizeCanvasBox);
  }
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      scheduleFirstScreenPin(false);
      refreshHealth();
      connect();
    }
  });
  window.addEventListener('pageshow', () => scheduleFirstScreenPin(true));
  window.addEventListener('load', () => scheduleFirstScreenPin(true));
  setInterval(() => send({ type: 'heartbeat' }), 15000);
  setInterval(refreshHealth, 15000);
  setInterval(() => {
    if (currentState && currentState.activeControl) renderState();
  }, 1000);
  setInterval(() => {
    if (streamUnsupported) return;
    if (!ws || ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING) {
      connect();
      return;
    }
    if (ws.readyState !== WebSocket.OPEN) return;
    const now = performance.now();
    if (!configured && connectedAt > 0 && now - connectedAt > streamStartupGraceMs) {
      if (now - lastFirstFrameNudgeAt > streamStartupGraceMs) {
        lastFirstFrameNudgeAt = now;
        requestKeyframe('h264_first_frame_nudge');
        clientLog('loading_over_2s', 'h264_first_frame_pending');
      }
      if (now - connectedAt > streamStartupHardErrorMs) {
        sendVideoClientLog('h264_start_timeout', 'missing_h264_config_or_frame');
        showUnsupported('Video straume neatnāca laikā. Tālrunim vajag uzmanību.');
      }
      return;
    }
    if (configured && lastFrameAt === 0 && configuredAt > 0 && now - configuredAt > streamStartupGraceMs) {
      requestKeyframe('first_frame_timeout');
      restartStream('first_frame_timeout');
      return;
    }
    if (lastFrameAt > 0 && now - lastFrameAt > streamWatchdogMs) {
      if (now - lastFirstFrameNudgeAt > streamWatchdogMs) {
        lastFirstFrameNudgeAt = now;
        requestKeyframe('stale_video_frames');
        clientLog('stale_video_frames', 'fresh_frame_requested');
      }
      if (now - lastFrameAt > streamStaleRestartMs) {
        restartStream('stale_video_frames');
      }
    }
  }, 1000);
  updateViewportVars();
  scheduleFirstScreenPin(true);
  updateDetailsReveal();
  resizeCanvasBox();
  showEmpty('Savienojas...', false);
  refreshHealth();
  connect();

  async function startAdmin() {
    const memberForm = document.getElementById('memberForm');
    const memberEmail = document.getElementById('memberEmail');
    const memberRole = document.getElementById('memberRole');
    const membersEl = document.getElementById('adminMembers');
    const stateEl = document.getElementById('adminState');
    const revokeButton = document.getElementById('adminRevoke');
    const notice = document.getElementById('adminNotice');
    const memberSummary = document.getElementById('adminMemberSummary');
    const sessionSummary = document.getElementById('adminSessionSummary');
    const phoneState = document.getElementById('adminPhoneState');
    const phoneDetail = document.getElementById('adminPhoneDetail');
    const streamState = document.getElementById('adminStreamState');
    const streamDetail = document.getElementById('adminStreamDetail');
    const controlState = document.getElementById('adminControlState');
    const controlDetail = document.getElementById('adminControlDetail');
    const safetyState = document.getElementById('adminSafetyState');
    const safetyDetail = document.getElementById('adminSafetyDetail');
    const backendSummary = document.getElementById('adminBackendSummary');
    const backendList = document.getElementById('adminBackendList');
    const simSetup = document.querySelector('[data-simulator-setup="true"]');
    const simSetupSummary = document.getElementById('simSetupSummary');
    const simSetupPackages = document.getElementById('simSetupPackages');
    const simSetupScreenshot = document.getElementById('simSetupScreenshot');
    const simSetupRefreshButton = document.getElementById('simSetupRefresh');
    const simSetupTextForm = document.getElementById('simSetupTextForm');
    const simSetupText = document.getElementById('simSetupText');
    const simSetupLastInput = document.getElementById('simSetupLastInput');
    let simSetupDisplay = { width: 720, height: 1280 };
    let simSetupPointer = null;
    let simSetupLongPressTimer = null;
    const simSetupTapMaxDistance = 12;
    const simSetupLongPressDelayMs = 650;

    async function load() {
      const [stateResponse, backendResponse] = await Promise.all([
        fetch('/api/v1/admin/state', { cache: 'no-store' }),
        fetch('/api/v1/admin/phone/backends', { cache: 'no-store' })
      ]);
      const payload = await stateResponse.json();
      const backendsPayload = await backendResponse.json();
      if (!stateResponse.ok || !payload.ok) throw new Error(payload.message || 'load failed');
      if (!backendResponse.ok || !backendsPayload.ok) throw new Error(backendsPayload.message || 'backend load failed');
      renderAdmin(payload.state, payload.phone, backendsPayload);
      if (simSetup) loadSimulatorSetup().catch((error) => renderSimulatorSetupError(error.message || 'Simulator control unavailable'));
    }

    function renderAdmin(state, phone, backendsPayload) {
      const phoneHealth = parsePhoneHealth(state.phone && state.phone.healthJson);
      renderStatus(state, phone, phoneHealth);
      renderBackends(backendsPayload);
      membersEl.textContent = '';
      (state.members || []).forEach((member) => {
        const row = document.createElement('div');
        row.className = 'admin-member';
        const main = document.createElement('div');
        main.className = 'admin-member-main';
        const email = document.createElement('span');
        email.className = 'admin-member-email';
        email.textContent = member.email;
        const updated = document.createElement('span');
        updated.className = 'admin-muted';
        updated.textContent = member.active === false ? 'Inactive' : relativeTime(member.updatedAt);
        main.append(email, updated);
        const role = document.createElement('span');
        role.className = `admin-pill ${member.role || 'member'}`;
        role.textContent = member.role;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.textContent = 'Remove';
        remove.disabled = member.role === 'owner';
        remove.addEventListener('click', async () => {
          await runAdminAction(remove, 'Removing member...', async () => {
            await apiFetch(`/api/v1/admin/members?email=${encodeURIComponent(member.email)}`, { method: 'DELETE' });
            showNotice('Member removed');
            await load();
          });
        });
        row.append(main, role, remove);
        membersEl.appendChild(row);
      });
      stateEl.textContent = JSON.stringify({ state, phone, phoneBackends: backendsPayload }, null, 2);
    }

    memberForm.addEventListener('submit', async (event) => {
      event.preventDefault();
      await runAdminAction(memberForm.querySelector('button[type="submit"]'), 'Adding member...', async () => {
        await apiFetch('/api/v1/admin/members', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ email: memberEmail.value, role: memberRole.value })
        });
        memberEmail.value = '';
        showNotice('Member saved');
        await load();
      });
    });

    revokeButton.addEventListener('click', async () => {
      await runAdminAction(revokeButton, 'Revoking control...', async () => {
        await apiFetch('/api/v1/admin/control/revoke', { method: 'POST' });
        showNotice('Control revoked');
        await load();
      });
    });

    function renderStatus(state, phone, phoneHealth) {
      const members = state.members || [];
      const viewers = state.viewers || [];
      const activeViewers = viewers.filter((viewer) => viewer.connected !== false);
      const activeControl = state.activeControl || null;
      const phoneRecord = state.phone || {};
      const rootCapture = phoneHealth.rootCapture || {};
      const pipeline = phoneHealth.streamPipeline || {};
      const inputGate = phoneHealth.inputGate || {};
      const lockdown = phoneHealth.notificationLockdown || {};

      memberSummary.textContent = `${members.length} member${members.length === 1 ? '' : 's'} configured`;
      sessionSummary.textContent = activeControl
        ? `${activeControl.email} has control for ${Math.max(0, Math.ceil((activeControl.remainingMs || 0) / 1000))}s`
        : 'No active control claim';

      phoneState.textContent = phone && phone.connected ? 'Connected' : phoneRecord.desiredState || 'Idle';
      phoneDetail.textContent = `${phoneRecord.attachName || phoneRecord.id || 'Pixel'} · seen ${relativeTime(phoneRecord.lastSeenAt || (phone && phone.lastSeenAt))}`;

      streamState.textContent = rootCapture.active ? 'Live' : (phoneHealth.streamActive ? 'Starting' : 'Idle');
      streamDetail.textContent = rootCapture.message || pipeline.secureWindowCaptureBypassMessage || 'Waiting for viewers';

      controlState.textContent = activeControl ? 'Claimed' : 'Open';
      controlDetail.textContent = activeControl
        ? `${activeControl.email}${activeControl.extended ? ' · extended' : ''}`
        : `${activeViewers.length} viewer${activeViewers.length === 1 ? '' : 's'} on page`;

      safetyState.textContent = lockdown.active ? 'Locked down' : 'Ready';
      safetyDetail.textContent = inputGate.reason
        ? `Input gate: ${inputGate.reason}`
        : (lockdown.reason || 'Tap-only controls');

      revokeButton.disabled = !activeControl;
      revokeButton.classList.toggle('is-danger', Boolean(activeControl));
    }

    function renderBackends(payload) {
      const activeId = payload.activeBackendId || '';
      const backends = payload.backends || [];
      const active = backends.find((backend) => backend.id === activeId);
      backendSummary.textContent = active
        ? `Active: ${active.attachName || active.id}`
        : 'No active backend selected';
      backendList.textContent = '';
      backends.forEach((backend) => {
        const row = document.createElement('div');
        row.className = `admin-backend ${backend.active ? 'active' : ''}`;
        const main = document.createElement('div');
        main.className = 'admin-backend-main';
        const name = document.createElement('strong');
        name.textContent = backend.attachName || backend.id;
        const detail = document.createElement('span');
        detail.className = 'admin-muted';
        const relay = backend.relay || {};
        const state = backend.active
          ? `${relay.streamState || 'idle'}${relay.connected ? ' · connected' : ''}`
          : (backend.healthOk ? 'reachable' : 'not reachable');
        detail.textContent = `${state} · ${backend.baseUrl || ''}`;
        main.append(name, detail);

        const badge = document.createElement('span');
        badge.className = `admin-pill ${backend.active ? 'owner' : ''}`;
        badge.textContent = backend.active ? 'active' : (backend.healthOk ? 'ready' : 'offline');

        const button = document.createElement('button');
        button.type = 'button';
        button.textContent = backend.active ? 'Selected' : 'Use';
        button.disabled = backend.active;
        button.addEventListener('click', async () => {
          await runAdminAction(button, 'Switching...', async () => {
            await apiFetch('/api/v1/admin/phone/backend', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ backendId: backend.id })
            });
            showNotice(`Switched to ${backend.attachName || backend.id}`);
            await load();
          });
        });

        row.append(main, badge, button);
        backendList.appendChild(row);
      });
    }

    async function loadSimulatorSetup() {
      if (!simSetup) return;
      const response = await fetch('/api/v1/admin/phone/setup/status', { cache: 'no-store' });
      const payload = await response.json();
      if (!response.ok || !payload.ok) throw new Error(payload.message || payload.error || 'Simulator control unavailable');
      renderSimulatorSetup(payload);
    }

    function renderSimulatorSetup(payload) {
      const display = payload.display || {};
      if (display.width && display.height) simSetupDisplay = display;
      const displayLabel = `${simSetupDisplay.width}x${simSetupDisplay.height}${simSetupDisplay.density ? ` · ${simSetupDisplay.density} dpi` : ''}`;
      simSetupSummary.textContent = payload.connected
        ? `Connected · ${displayLabel}${payload.message ? ` · ${payload.message}` : ''}`
        : payload.error || 'Simulator is not connected';
      simSetupPackages.textContent = '';
      const packages = payload.packages || {};
      [
        ['vivi', 'ViVi'],
        ['accrescent', 'Accrescent'],
        ['aurora', 'Aurora'],
        ['controller', 'Controller']
      ].forEach(([key, label]) => {
        const info = packages[key] || {};
        const pill = document.createElement('span');
        pill.className = `admin-pill ${info.installed ? 'owner' : ''}`;
        pill.textContent = `${label}: ${info.installed ? 'installed' : 'missing'}`;
        simSetupPackages.appendChild(pill);
      });
      if (payload.connected && simSetupScreenshot) {
        refreshSimulatorScreenshot();
      }
    }

    function renderSimulatorSetupError(message) {
      if (!simSetupSummary) return;
      simSetupSummary.textContent = message;
    }

    function refreshSimulatorScreenshot(delayMs) {
      if (!simSetupScreenshot) return;
      const refresh = () => {
        simSetupScreenshot.src = `/api/v1/admin/phone/setup/screenshot?t=${Date.now()}`;
      };
      if (delayMs && delayMs > 0) {
        setTimeout(refresh, delayMs);
        return;
      }
      refresh();
    }

    function setSimulatorLastInput(message, failed) {
      if (!simSetupLastInput) return;
      simSetupLastInput.textContent = message;
      simSetupLastInput.classList.toggle('admin-error', Boolean(failed));
    }

    async function postSimulatorInput(body, label) {
      await apiFetch('/api/v1/admin/phone/setup/input', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      setSimulatorLastInput(label || 'Input sent');
      refreshSimulatorScreenshot();
      refreshSimulatorScreenshot(450);
      refreshSimulatorScreenshot(1100);
      setTimeout(() => loadSimulatorSetup().catch((error) => renderSimulatorSetupError(error.message || 'Simulator control unavailable')), 1200);
    }

    function simulatorScreenPoint(event) {
      if (!simSetupScreenshot) return null;
      const rect = simSetupScreenshot.getBoundingClientRect();
      if (!rect.width || !rect.height) return null;
      const width = simSetupDisplay.width || simSetupScreenshot.naturalWidth || rect.width;
      const height = simSetupDisplay.height || simSetupScreenshot.naturalHeight || rect.height;
      const x = Math.max(0, Math.min(width - 1, Math.round(((event.clientX - rect.left) / rect.width) * width)));
      const y = Math.max(0, Math.min(height - 1, Math.round(((event.clientY - rect.top) / rect.height) * height)));
      return { x, y, at: Date.now() };
    }

    function simulatorPointDistance(a, b) {
      if (!a || !b) return 0;
      const dx = a.x - b.x;
      const dy = a.y - b.y;
      return Math.sqrt(dx * dx + dy * dy);
    }

    function clearSimulatorLongPressTimer() {
      if (simSetupLongPressTimer) {
        clearTimeout(simSetupLongPressTimer);
        simSetupLongPressTimer = null;
      }
    }

    function simulatorGestureDuration(start, end) {
      return Math.max(50, Math.min(1000, Math.round(((end && end.at) || Date.now()) - ((start && start.at) || Date.now()))));
    }

    function simulatorKeyInput(event) {
      if (event.ctrlKey || event.metaKey || event.altKey) return null;
      switch (event.key) {
        case 'Backspace':
          return { body: { type: 'key', key: 'delete' }, label: 'Delete sent' };
        case 'Enter':
          return { body: { type: 'key', key: 'enter' }, label: 'Enter sent' };
        case ' ':
        case 'Spacebar':
          return { body: { type: 'key', key: 'space' }, label: 'Space sent' };
        case 'Tab':
          return { body: { type: 'key', key: 'tab' }, label: 'Tab sent' };
        case 'Escape':
          return { body: { type: 'key', key: 'escape' }, label: 'Escape sent' };
        default:
          break;
      }
      if (event.key && event.key.length === 1) {
        return { body: { type: 'text', text: event.key }, label: 'Text sent' };
      }
      return null;
    }

    if (simSetup) {
      if (simSetupRefreshButton) {
        simSetupRefreshButton.addEventListener('click', async () => {
          await runAdminAction(simSetupRefreshButton, 'Refreshing...', async () => {
            await loadSimulatorSetup();
            refreshSimulatorScreenshot();
            setSimulatorLastInput('Screen refreshed');
          });
        });
      }
      simSetup.querySelectorAll('[data-sim-open]').forEach((button) => {
        button.addEventListener('click', async () => {
          await runAdminAction(button, 'Opening...', async () => {
            await apiFetch('/api/v1/admin/phone/setup/open', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ target: button.dataset.simOpen })
            });
            setSimulatorLastInput(`${button.textContent.trim()} opened`);
            refreshSimulatorScreenshot();
            refreshSimulatorScreenshot(650);
            setTimeout(() => loadSimulatorSetup().catch((error) => renderSimulatorSetupError(error.message || 'Simulator control unavailable')), 900);
          });
        });
      });
      simSetup.querySelectorAll('[data-sim-key]').forEach((button) => {
        button.addEventListener('click', async () => {
          await runAdminAction(button, 'Sending...', async () => {
            await postSimulatorInput({ type: 'key', key: button.dataset.simKey }, `${button.textContent.trim()} sent`);
          });
        });
      });
      if (simSetupTextForm) {
        simSetupTextForm.addEventListener('submit', async (event) => {
          event.preventDefault();
          await runAdminAction(simSetupTextForm.querySelector('button[type="submit"]'), 'Typing...', async () => {
            await postSimulatorInput({ type: 'text', text: simSetupText.value }, 'Text sent');
            simSetupText.value = '';
          });
        });
      }
      if (simSetupScreenshot) {
        simSetupScreenshot.addEventListener('pointerdown', (event) => {
          if (event.button !== undefined && event.button !== 0) return;
          const point = simulatorScreenPoint(event);
          if (!point) return;
          event.preventDefault();
          simSetupScreenshot.focus({ preventScroll: true });
          try { simSetupScreenshot.setPointerCapture(event.pointerId); } catch (_) {}
          simSetupPointer = {
            id: event.pointerId,
            start: point,
            last: point,
            longPressSent: false
          };
          clearSimulatorLongPressTimer();
          simSetupLongPressTimer = setTimeout(() => {
            if (!simSetupPointer || simSetupPointer.id !== event.pointerId) return;
            simSetupPointer.longPressSent = true;
            postSimulatorInput({ type: 'long_press', x: point.x, y: point.y, durationMs: simSetupLongPressDelayMs }, 'Long press sent')
              .catch((error) => {
                setSimulatorLastInput(error.message || 'Long press failed', true);
                showNotice(error.message || 'Long press failed', true);
              });
          }, simSetupLongPressDelayMs);
        });
        simSetupScreenshot.addEventListener('pointermove', (event) => {
          if (!simSetupPointer || simSetupPointer.id !== event.pointerId) return;
          const point = simulatorScreenPoint(event);
          if (!point) return;
          simSetupPointer.last = point;
          if (simulatorPointDistance(simSetupPointer.start, point) > simSetupTapMaxDistance) {
            clearSimulatorLongPressTimer();
          }
        });
        simSetupScreenshot.addEventListener('pointerup', async (event) => {
          if (!simSetupPointer || simSetupPointer.id !== event.pointerId) return;
          event.preventDefault();
          clearSimulatorLongPressTimer();
          const pointer = simSetupPointer;
          simSetupPointer = null;
          try { simSetupScreenshot.releasePointerCapture(event.pointerId); } catch (_) {}
          if (pointer.longPressSent) return;
          const end = simulatorScreenPoint(event) || pointer.last || pointer.start;
          const distance = simulatorPointDistance(pointer.start, end);
          const body = distance > simSetupTapMaxDistance
            ? { type: 'drag', startX: pointer.start.x, startY: pointer.start.y, endX: end.x, endY: end.y, durationMs: simulatorGestureDuration(pointer.start, end) }
            : { type: 'tap', x: end.x, y: end.y };
          const label = distance > simSetupTapMaxDistance ? 'Swipe sent' : 'Tap sent';
          await postSimulatorInput(body, label).catch((error) => {
            setSimulatorLastInput(error.message || `${label} failed`, true);
            showNotice(error.message || `${label} failed`, true);
          });
        });
        simSetupScreenshot.addEventListener('pointercancel', (event) => {
          if (simSetupPointer && simSetupPointer.id === event.pointerId) {
            simSetupPointer = null;
            clearSimulatorLongPressTimer();
          }
        });
        simSetupScreenshot.addEventListener('keydown', async (event) => {
          const input = simulatorKeyInput(event);
          if (!input) return;
          event.preventDefault();
          await postSimulatorInput(input.body, input.label).catch((error) => {
            setSimulatorLastInput(error.message || 'Keyboard input failed', true);
            showNotice(error.message || 'Keyboard input failed', true);
          });
        });
      }
      setInterval(() => {
        loadSimulatorSetup().catch((error) => renderSimulatorSetupError(error.message || 'Simulator control unavailable'));
      }, 3500);
    }

    function parsePhoneHealth(raw) {
      if (!raw) return {};
      try {
        const parsed = JSON.parse(raw);
        return parsed && parsed.data ? parsed.data : parsed;
      } catch (_) {
        return {};
      }
    }

    function relativeTime(value) {
      if (!value) return 'never';
      const at = Date.parse(value);
      if (!Number.isFinite(at)) return value;
      const seconds = Math.max(0, Math.round((Date.now() - at) / 1000));
      if (seconds < 5) return 'just now';
      if (seconds < 60) return `${seconds}s ago`;
      const minutes = Math.round(seconds / 60);
      if (minutes < 60) return `${minutes}m ago`;
      const hours = Math.round(minutes / 60);
      if (hours < 24) return `${hours}h ago`;
      const days = Math.round(hours / 24);
      return `${days}d ago`;
    }

    function showNotice(message, error = false) {
      notice.textContent = message;
      notice.classList.toggle('error', error);
      notice.hidden = false;
    }

    async function apiFetch(url, options) {
      const response = await fetch(url, options);
      if (response.ok) return response;
      let message = `${response.status} ${response.statusText}`.trim();
      try {
        const payload = await response.json();
        message = payload.message || payload.error || message;
      } catch (_) {}
      throw new Error(message);
    }

    async function runAdminAction(button, pending, action) {
      const original = button ? button.textContent : '';
      const wasDisabled = button ? button.disabled : false;
      try {
        if (button) {
          button.disabled = true;
          button.textContent = pending;
        }
        await action();
      } catch (error) {
        showNotice(error.message || 'Action failed', true);
      } finally {
        if (button) {
          button.textContent = original;
          button.disabled = wasDisabled || (button.id === 'adminRevoke' && !button.classList.contains('is-danger'));
        }
      }
    }

    load().catch((error) => {
      showNotice(error.message || 'Load failed', true);
      stateEl.textContent = error.stack || error.message;
    });
  }
})();
