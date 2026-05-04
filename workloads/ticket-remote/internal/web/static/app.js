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

  const canvas = document.getElementById('screen');
  const ctx = canvas.getContext('2d');
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
  const claimDialog = document.getElementById('claimDialog');

  let ws = null;
  let videoWs = null;
  let reconnectTimer = null;
  let decoder = null;
  let configured = false;
  let decoderUnsupported = false;
  let needsKeyFrame = true;
  let streamSize = { width: 540, height: 1080 };
  let currentState = null;
  let serverClockSkewMs = 0;
  let pointerStart = null;
  let connectedAt = 0;
  let configuredAt = 0;
  let lastFrameAt = 0;
  let lastRestartAt = 0;
  const maxTapDurationMs = 450;
  const maxTapTravelPx = 14;
  const streamWatchdogMs = 4500;
  const streamStartupGraceMs = 9000;

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

  document.body.dataset.videoPath = 'websocket';

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
    ['Ticket session is active through MediaProjection fallback', 'Biļetes sesija darbojas ar MediaProjection rezerves režīmu'],
    ['Root capture is idle', 'Root ekrāna tveršana ir gaidstāvē'],
    ['Root shell is unavailable', 'Root komandrinda nav pieejama'],
    ['Root screenrecord capture is available', 'Root ekrāna tveršana ir pieejama'],
    ['Root capture is starting', 'Root ekrāna tveršana startējas'],
    ['Root capture is active', 'Root ekrāna tveršana ir aktīva'],
    ['Root capture is unavailable', 'Root ekrāna tveršana nav pieejama'],
    ['Root capture is unavailable and MediaProjection AV1 fallback is unavailable', 'Root ekrāna tveršana nav pieejama, un MediaProjection AV1 rezerves režīms arī nav pieejams'],
    ['Screen capture permission is waiting on the Pixel', 'Pixel gaida ekrāna tveršanas atļauju'],
    ['Screen capture permission is already pending on the Pixel', 'Pixel jau gaida ekrāna tveršanas atļauju'],
    ['Screen capture permission could not be opened on the Pixel', 'Pixel nevarēja atvērt ekrāna tveršanas atļauju'],
    ['Screen capture permission was not granted', 'Ekrāna tveršanas atļauja netika piešķirta'],
    ['Screen capture permission was rejected by Android', 'Android noraidīja ekrāna tveršanas atļauju'],
    ['Screen capture permission is ready', 'Ekrāna tveršanas atļauja ir gatava'],
    ['Screen capture permission is not ready', 'Ekrāna tveršanas atļauja vēl nav gatava'],
    ['Screen capture stopped by Android', 'Android apturēja ekrāna tveršanu'],
    ['Hardware AV1 encoder is unavailable', 'Aparatūras AV1 kodētājs nav pieejams'],
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
      ['Root capture failed: ', 'Root ekrāna tveršana neizdevās: '],
      ['AV1 stream stopped: ', 'AV1 straume apturēta: ']
    ]) {
      if (text.startsWith(prefix)) return translation + text.slice(prefix.length);
    }
    return text;
  }

  function setStatus(text) {
    statusLine.textContent = localizePublicMessage(text);
  }

  function clientLog(event, detail) {
    fetch('/api/v1/client-log', {
      method: 'POST',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        event,
        pageVersion,
        detail: String(detail || '').slice(0, 500),
        videoDecoder: 'VideoDecoder' in window,
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
    decoderUnsupported = true;
    configured = false;
    showEmpty(message, false);
    setStatus(message);
    clientLog('decoder_unsupported', message);
  }

  function resizeCanvasBox() {
    updateViewportVars();
    const stage = document.querySelector('.stage');
    const maxWidth = Math.max(1, stage.clientWidth);
    const maxHeight = Math.max(1, stage.clientHeight);
    const scale = Math.min(maxWidth / streamSize.width, maxHeight / streamSize.height);
    canvas.style.setProperty('--stream-width', `${Math.max(1, Math.floor(streamSize.width * scale))}px`);
    canvas.style.setProperty('--stream-height', `${Math.max(1, Math.floor(streamSize.height * scale))}px`);
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
      if (!decoderUnsupported) {
        showEmpty(configured ? 'Gaida tiešraides kadru...' : 'Gaida biļetes straumi...', false);
      }
      send({ type: 'heartbeat' });
      connectDirectVideo();
    };
    ws.onmessage = handleMessage;
    ws.onclose = () => {
      setConnected('Savienojas no jauna');
      configured = false;
      decoderUnsupported = false;
      keepFirstScreenPinned();
      if (decoder) {
        try { decoder.close(); } catch (_) {}
        decoder = null;
      }
      closeDirectVideo();
      showEmpty('Atjauno straumi...', false);
      reconnectTimer = setTimeout(connect, 1000);
    };
    ws.onerror = () => {
      setConnected('Savienojuma kļūme');
      if (!decoderUnsupported) {
        showEmpty('Atjauno straumi...', false);
      }
      clientLog('websocket_error', 'socket error');
    };
  }

  function resetDecoder() {
    configured = false;
    needsKeyFrame = true;
    configuredAt = 0;
    lastFrameAt = 0;
    if (decoder) {
      try { decoder.close(); } catch (_) {}
      decoder = null;
    }
  }

  function restartStream(reason) {
    if (decoderUnsupported) return;
    const now = performance.now();
    if (now - lastRestartAt < 5000) return;
    lastRestartAt = now;
    clientLog('stream_restart', reason);
    closeDirectVideo();
    resetDecoder();
    showEmpty('Atjauno straumi...', false);
    if (ws) {
      try { ws.close(4000, reason); } catch (_) {}
      ws = null;
    }
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(connect, 250);
  }

  function closeDirectVideo() {
    if (videoWs) {
      try { videoWs.close(); } catch (_) {}
      videoWs = null;
    }
  }

  function connectDirectVideo() {
    if (videoWs && (videoWs.readyState === WebSocket.OPEN || videoWs.readyState === WebSocket.CONNECTING)) return;
    closeDirectVideo();
    document.body.dataset.videoPath = 'websocket';
    videoWs = new WebSocket(streamURL());
    videoWs.binaryType = 'arraybuffer';
    videoWs.onopen = () => {
      videoWs.send(JSON.stringify({ type: 'keyframe' }));
    };
    videoWs.onmessage = handleMessage;
    videoWs.onclose = () => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        setTimeout(connectDirectVideo, 1000);
      }
    };
    videoWs.onerror = () => {
      clientLog('direct_video_websocket_error', 'socket error');
    };
  }

  function send(value) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(value));
      return true;
    }
    return false;
  }

  async function handleMessage(event) {
    if (typeof event.data === 'string') {
      let msg;
      try { msg = JSON.parse(event.data); } catch (_) { return; }
      if (msg.type === 'config') {
        await configureDecoder(msg);
      } else if (msg.type === 'state') {
        currentState = msg.state;
        rememberServerClock(currentState);
        renderState();
      } else if (msg.type === 'health') {
        if (msg.data && msg.data.message) {
          setStatus(msg.data.message);
          if (msg.data.streamActive === false && !decoderUnsupported) {
            showEmpty(`${localizePublicMessage(msg.data.message)} Restartē...`, false);
          }
        }
      } else if (msg.type === 'phone') {
        setStatus(msg.message || '');
      } else if (msg.type === 'input' && msg.accepted === false) {
        setStatus('Ievade netiek pieņemta, kamēr nav pārņemts kontroles koda režīms.');
      }
      return;
    }
    if (!configured || !decoder) return;
    if (isPrivacyCovered()) return;
    const data = new Uint8Array(event.data);
    const view = new DataView(event.data);
    const kind = data[0] === 1 ? 'key' : 'delta';
    if (needsKeyFrame && kind !== 'key') return;
    if (kind === 'key') needsKeyFrame = false;
    const high = view.getUint32(1);
    const low = view.getUint32(5);
    const timestamp = high * 4294967296 + low;
    try {
      decoder.decode(new EncodedVideoChunk({ type: kind, timestamp, data: data.slice(9) }));
    } catch (error) {
      configured = false;
      showUnsupported(`${kind === 'key' ? 'Atslēgkadra' : 'Kadra'} atkodēšana neizdevās: ${error.message || error.name || 'dekoderis noraidīja kadru'}`);
    }
  }

  async function configureDecoder(config) {
    decoderUnsupported = false;
    if (!('VideoDecoder' in window)) {
      showUnsupported('Šī pārlūkprogramma nevar atkodēt biļetes straumi.');
      return;
    }
    const h264 = String(config.codec || '').startsWith('avc1') || config.transport === 'h264-annexb';
    const decoderConfig = { codec: config.codec, codedWidth: config.width, codedHeight: config.height };
    if (h264) decoderConfig.avc = { format: 'annexb' };
    let supported;
    try {
      supported = await VideoDecoder.isConfigSupported(decoderConfig);
    } catch (error) {
      showUnsupported(`Video atbalsta pārbaude neizdevās: ${error.message || error.name || 'neatbalstīts kodeks'}`);
      return;
    }
    if (!supported.supported) {
      showUnsupported(h264 ? 'Šī pārlūkprogramma nevar atkodēt H.264.' : 'Šī pārlūkprogramma nevar atkodēt AV1.');
      return;
    }
    if (decoder) {
      try { decoder.close(); } catch (_) {}
    }
    configuredAt = performance.now();
    lastFrameAt = 0;
    canvas.width = config.width;
    canvas.height = config.height;
    streamSize = { width: config.width, height: config.height };
    resizeCanvasBox();
    decoder = new VideoDecoder({
      output(frame) {
        ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
        frame.close();
        lastFrameAt = performance.now();
        hideEmpty();
      },
      error(error) {
        showUnsupported(`${h264 ? 'H.264' : 'AV1'} dekodera kļūda: ${error.message || 'atkodēšana neizdevās'}`);
      }
    });
    try {
      decoder.configure(supported.config || decoderConfig);
    } catch (error) {
      showUnsupported(`${h264 ? 'H.264' : 'AV1'} dekodera palaišana neizdevās: ${error.message || error.name || 'neatbalstīts kodeks'}`);
      return;
    }
    needsKeyFrame = true;
    configured = true;
    showEmpty(h264 ? 'Gaida pirmo H.264 kadru...' : 'Gaida pirmo AV1 kadru...', false);
    send({ type: 'keyframe' });
    keepFirstScreenPinned();
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

  function claimControl() {
    if (claimDialog.open || claimDialog.hasAttribute('open')) return;
    if (typeof claimDialog.showModal === 'function') {
      claimDialog.showModal();
    } else if (confirm('Pārņemt privātu kontroles koda režīmu uz 45 sekundēm?')) {
      postJSON('/api/v1/control/claim').catch((error) => setStatus(error.message));
    }
  }

  claimDialog.addEventListener('close', () => {
    if (claimDialog.returnValue === 'claim') {
      postJSON('/api/v1/control/claim').catch((error) => setStatus(error.message));
    }
  });

  claimButton.addEventListener('click', claimControl);
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

  function isStreamControleButton(screenPoint) {
    const width = canvas.width || streamSize.width || 1;
    const height = canvas.height || streamSize.height || 1;
    const relativeX = screenPoint.x / width;
    const relativeY = screenPoint.y / height;
    return relativeX >= 0.02 && relativeX <= 0.40 && relativeY >= 0.09 && relativeY <= 0.17;
  }

  canvas.addEventListener('pointerdown', (event) => {
    if (!configured || isPrivacyCovered()) return;
    const control = currentControl(currentState);
    const start = point(event);
    if (!control || control.sessionId !== cfg.sessionId || control.email !== cfg.email) {
      if (!control && isStreamControleButton(start)) {
        event.preventDefault();
        pointerStart = null;
        claimControl();
        return;
      }
      setStatus('Pirms pieskaries tālrunim, pārņem kontroles koda režīmu.');
      return;
    }
    pointerStart = { ...start, at: performance.now() };
    canvas.setPointerCapture(event.pointerId);
  });

  canvas.addEventListener('pointerup', (event) => {
    if (!pointerStart || !configured || isPrivacyCovered()) return;
    const end = point(event);
    const distance = Math.hypot(end.x - pointerStart.x, end.y - pointerStart.y);
    const heldMs = performance.now() - pointerStart.at;
    if (distance < maxTapTravelPx && heldMs <= maxTapDurationMs) {
      send({ type: 'tap', x: end.x, y: end.y });
    } else {
      setStatus('Atbalstīti ir tikai pieskārieni.');
      clientLog('blocked_gesture', distance < maxTapTravelPx ? 'long_press' : 'swipe');
    }
    pointerStart = null;
  });

  canvas.addEventListener('pointercancel', () => {
    pointerStart = null;
  });
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
    if (decoderUnsupported) return;
    if (!ws || ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING) {
      connect();
      return;
    }
    if (ws.readyState !== WebSocket.OPEN) return;
    const now = performance.now();
    if (!configured && connectedAt > 0 && now - connectedAt > streamStartupGraceMs) {
      restartStream('missing_stream_config');
      return;
    }
    if (configured && lastFrameAt === 0 && configuredAt > 0 && now - configuredAt > streamStartupGraceMs) {
      send({ type: 'keyframe' });
      restartStream('first_frame_timeout');
      return;
    }
    if (lastFrameAt > 0 && now - lastFrameAt > streamWatchdogMs) {
      restartStream('stale_video_frames');
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

    async function load() {
      const response = await fetch('/api/v1/admin/state', { cache: 'no-store' });
      const payload = await response.json();
      if (!response.ok || !payload.ok) throw new Error(payload.message || 'load failed');
      renderAdmin(payload.state, payload.phone);
    }

    function renderAdmin(state, phone) {
      const phoneHealth = parsePhoneHealth(state.phone && state.phone.healthJson);
      renderStatus(state, phone, phoneHealth);
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
      stateEl.textContent = JSON.stringify({ state, phone }, null, 2);
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
