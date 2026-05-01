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

  function setStatus(text) {
    statusLine.textContent = text || '';
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
    emptyMessage.textContent = message || '';
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
    setConnected('Connecting');
    connectedAt = performance.now();
    ws = new WebSocket(socketURL());
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => {
      setConnected('Connected');
      if (!decoderUnsupported) {
        showEmpty(configured ? 'Waiting for live frame...' : 'Waiting for ticket stream...', false);
      }
      send({ type: 'heartbeat' });
      connectDirectVideo();
    };
    ws.onmessage = handleMessage;
    ws.onclose = () => {
      setConnected('Reconnecting');
      configured = false;
      decoderUnsupported = false;
      keepFirstScreenPinned();
      if (decoder) {
        try { decoder.close(); } catch (_) {}
        decoder = null;
      }
      closeDirectVideo();
      showEmpty('Reconnecting stream...', false);
      reconnectTimer = setTimeout(connect, 1000);
    };
    ws.onerror = () => {
      setConnected('Connection issue');
      if (!decoderUnsupported) {
        showEmpty('Reconnecting stream...', false);
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
    showEmpty('Reconnecting stream...', false);
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
            showEmpty(`${msg.data.message} Restarting...`, false);
          }
        }
      } else if (msg.type === 'phone') {
        setStatus(msg.message || '');
      } else if (msg.type === 'input' && msg.accepted === false) {
        setStatus('Input ignored until you claim controle code mode.');
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
      showUnsupported(`${kind === 'key' ? 'Keyframe' : 'Frame'} decode failed here: ${error.message || error.name || 'decoder rejected the frame'}`);
    }
  }

  async function configureDecoder(config) {
    decoderUnsupported = false;
    if (!('VideoDecoder' in window)) {
      showUnsupported('This browser cannot decode the ticket stream.');
      return;
    }
    const h264 = String(config.codec || '').startsWith('avc1') || config.transport === 'h264-annexb';
    const decoderConfig = { codec: config.codec, codedWidth: config.width, codedHeight: config.height };
    if (h264) decoderConfig.avc = { format: 'annexb' };
    let supported;
    try {
      supported = await VideoDecoder.isConfigSupported(decoderConfig);
    } catch (error) {
      showUnsupported(`Video support check failed here: ${error.message || error.name || 'unsupported codec'}`);
      return;
    }
    if (!supported.supported) {
      showUnsupported(h264 ? 'This browser cannot decode H.264 here.' : 'This browser cannot decode AV1 here.');
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
        showUnsupported(`${h264 ? 'H.264' : 'AV1'} decoder error here: ${error.message || 'decode failed'}`);
      }
    });
    try {
      decoder.configure(supported.config || decoderConfig);
    } catch (error) {
      showUnsupported(`${h264 ? 'H.264' : 'AV1'} decoder setup failed here: ${error.message || error.name || 'unsupported codec'}`);
      return;
    }
    needsKeyFrame = true;
    configured = true;
    showEmpty(h264 ? 'Waiting for first H.264 frame...' : 'Waiting for first AV1 frame...', false);
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
      setStatus(selfControl ? 'You have private phone control.' : `${control.email} has private phone control.`);
    } else {
      timer.hidden = true;
      timer.classList.remove('urgent');
      setStatus('General viewing');
    }

    if (otherControl) {
      privacyOverlay.hidden = false;
      privacyText.textContent = `${control.email} is using a private controle-code session for ${timer.textContent}. You stay connected and return to the shared view automatically.`;
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
    title.textContent = `${viewers.length} on page`;
    presence.appendChild(title);
    viewers.forEach((viewer) => {
      const row = document.createElement('div');
      row.className = 'presence-item';
      const email = document.createElement('span');
      email.textContent = viewer.email;
      const mark = document.createElement('span');
      mark.textContent = viewer.sessionId === cfg.sessionId ? 'you' : 'viewing';
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
      payload = { ok: false, message: text || 'request failed' };
    }
    if (!response.ok || !payload.ok) {
      throw new Error(payload.message || payload.error || 'request failed');
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
    } else if (confirm('Claim private controle-code mode for 45 seconds?')) {
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
      setStatus('Claim controle code mode before touching the phone.');
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
      setStatus('Only taps are supported.');
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
  showEmpty('Connecting...', false);
  refreshHealth();
  connect();

  async function startAdmin() {
    const memberForm = document.getElementById('memberForm');
    const memberEmail = document.getElementById('memberEmail');
    const memberRole = document.getElementById('memberRole');
    const membersEl = document.getElementById('adminMembers');
    const stateEl = document.getElementById('adminState');
    const revokeButton = document.getElementById('adminRevoke');

    async function load() {
      const response = await fetch('/api/v1/admin/state', { cache: 'no-store' });
      const payload = await response.json();
      if (!response.ok || !payload.ok) throw new Error(payload.message || 'load failed');
      renderAdmin(payload.state, payload.phone);
    }

    function renderAdmin(state, phone) {
      membersEl.textContent = '';
      (state.members || []).forEach((member) => {
        const row = document.createElement('div');
        row.className = 'admin-member';
        const email = document.createElement('span');
        email.textContent = member.email;
        const role = document.createElement('span');
        role.textContent = member.role;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.textContent = 'Remove';
        remove.disabled = member.role === 'owner';
        remove.addEventListener('click', async () => {
          await fetch(`/api/v1/admin/members?email=${encodeURIComponent(member.email)}`, { method: 'DELETE' });
          load().catch((error) => { stateEl.textContent = error.message; });
        });
        row.append(email, role, remove);
        membersEl.appendChild(row);
      });
      stateEl.textContent = JSON.stringify({ state, phone }, null, 2);
    }

    memberForm.addEventListener('submit', async (event) => {
      event.preventDefault();
      await fetch('/api/v1/admin/members', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: memberEmail.value, role: memberRole.value })
      });
      memberEmail.value = '';
      load().catch((error) => { stateEl.textContent = error.message; });
    });

    revokeButton.addEventListener('click', async () => {
      await fetch('/api/v1/admin/control/revoke', { method: 'POST' });
      load().catch((error) => { stateEl.textContent = error.message; });
    });

    load().catch((error) => { stateEl.textContent = error.message; });
  }
})();
