// Simulator controls (ROUTE_SPEC.md 6.10): pick a scenario, start or stop it,
// change the order rate while it runs.
//
// routed serves /sim/* only when it has a simulator wired in. On a 404 this
// file hides the panel and stops polling. Any other failure means the
// simulator is down, and the panel says so. The API token is typed in by the
// person at the keyboard and kept only in the input field: the dashboard
// bundles no secret, and nothing is written to storage.

(function () {
    const POLL_MS = 2000;
    const RATE_DEBOUNCE_MS = 300;
    const KEEP_ERROR_MS = 6000; // an error from a button press outlives the status poll
    const KEEP_DONE_MS = 3000;

    const panel = document.getElementById('sim-controls');
    const stateEl = document.getElementById('sim-state');
    const tokenEl = document.getElementById('sim-token');
    const tokenErrorEl = document.getElementById('sim-token-error');
    const profileEl = document.getElementById('sim-profile');
    const startEl = document.getElementById('sim-start');
    const stopEl = document.getElementById('sim-stop');
    const rateEl = document.getElementById('sim-rate');
    const rateOutEl = document.getElementById('sim-rate-out');
    const messageEl = document.getElementById('sim-message');
    if (!panel) return;

    // The panel is open on a wide screen and closed on a narrow one, where it
    // would cover the map. It follows the breakpoint when the window crosses it,
    // and otherwise stays as the person left it.
    const wide = window.matchMedia('(min-width: 768px)');
    panel.open = wide.matches;
    wide.addEventListener('change', (e) => { panel.open = e.matches; });

    let online = false;
    let running = false;
    let busy = false;
    let profilesLoaded = false;
    let rateTimer = null;
    let keepMessageUntil = 0;

    const message = (text, keepMs) => {
        messageEl.textContent = text;
        keepMessageUntil = keepMs ? Date.now() + keepMs : 0;
    };

    // A token problem is shown under the token field, where the fix is.
    const tokenError = (text) => {
        tokenErrorEl.textContent = text;
        if (text) tokenEl.setAttribute('aria-invalid', 'true');
        else tokenEl.removeAttribute('aria-invalid');
    };

    async function call(path, options) {
        const res = await fetch('/sim' + path, options);
        let body = null;
        try { body = await res.json(); } catch (e) { /* an empty or non-JSON body */ }
        return { res, body };
    }

    function render() {
        const usable = online && !busy;
        profileEl.disabled = !online || running || profileEl.options.length === 0;
        startEl.disabled = !usable || running || profileEl.options.length === 0;
        stopEl.disabled = !usable || !running;
        rateEl.disabled = !online || !running;
    }

    function showOffline() {
        online = false;
        running = false;
        stateEl.textContent = 'offline';
        message('No simulator controls are available. Start the simulator with: make sim-control');
        render();
    }

    function showStatus(st) {
        online = true;
        running = !!st.running;
        stateEl.textContent = running ? 'running ' + st.profile : 'idle';
        if (Date.now() >= keepMessageUntil) message('');
        const max = Math.min(st.max_rate || 600, Number(rateEl.max));
        rateEl.max = String(max);
        // Do not move the slider under the person's hand.
        if (st.orders_per_s != null && document.activeElement !== rateEl) {
            rateEl.value = String(Math.min(st.orders_per_s, max));
            rateOutEl.textContent = String(Math.round(st.orders_per_s));
        }
        if (running && !profileEl.matches(':focus')) profileEl.value = st.profile;
        render();
    }

    async function loadProfiles() {
        const { res, body } = await call('/profiles');
        if (!res.ok || !body || !Array.isArray(body.profiles)) return;
        profileEl.replaceChildren(...body.profiles.map((name) => new Option(name, name)));
        if (body.profiles.includes('demo')) profileEl.value = 'demo';
        profilesLoaded = true;
    }

    async function poll() {
        if (document.hidden) { // nobody is looking; check again shortly
            setTimeout(poll, POLL_MS);
            return;
        }
        try {
            const { res, body } = await call('/status');
            if (res.status === 404) {
                panel.hidden = true; // routed has no simulator wired in
                return;
            }
            panel.hidden = false;
            if (!res.ok) {
                showOffline();
            } else {
                if (!profilesLoaded) await loadProfiles();
                showStatus(body);
            }
        } catch (e) {
            panel.hidden = false;
            showOffline();
        }
        setTimeout(poll, POLL_MS);
    }

    // POST with the token. doneText is announced on success. Returns the new
    // status on success, null otherwise.
    async function act(path, payload, doneText) {
        const token = tokenEl.value.trim();
        if (!token) {
            tokenError('Enter the API token.');
            tokenEl.focus();
            return null;
        }
        const focused = document.activeElement;
        busy = true;
        render();
        try {
            const { res, body } = await call(path, {
                method: 'POST',
                headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
                body: payload ? JSON.stringify(payload) : undefined,
            });
            if (res.status === 401) {
                tokenError('That token was not accepted. Check it and try again.');
                tokenEl.focus();
            } else if (res.status === 429) {
                message('Too many requests. Wait a moment, then try again.', KEEP_ERROR_MS);
            } else if (!res.ok) {
                message((body && body.error ? body.error : 'The request failed') + '. Try again.', KEEP_ERROR_MS);
            } else {
                tokenError('');
                message(doneText || '', KEEP_DONE_MS);
                showStatus(body);
                return body;
            }
        } catch (e) {
            message('Could not reach the server. Try again.', KEEP_ERROR_MS);
        } finally {
            busy = false;
            render();
            // Disabling the button that was pressed drops keyboard focus to the
            // page. Give it back: to the same control if it is usable again,
            // otherwise to the next logical one. A disabled element can stay
            // document.activeElement for a moment, so check for that too.
            if (focused && focused !== document.body && focused !== tokenEl
                && (focused.disabled || document.activeElement !== focused)) {
                (focused.disabled ? (running ? stopEl : startEl) : focused).focus();
            }
        }
        return null;
    }

    tokenEl.addEventListener('input', () => tokenError(''));

    startEl.addEventListener('click', () => {
        const name = profileEl.value;
        if (name) act('/profiles/' + encodeURIComponent(name) + '/start', null, 'Started ' + name + '.');
    });
    stopEl.addEventListener('click', () => act('/stop', null, 'Stopped.'));

    rateEl.addEventListener('input', () => {
        rateOutEl.textContent = rateEl.value;
        clearTimeout(rateTimer);
        rateTimer = setTimeout(() => act('/rate', { orders_per_s: Number(rateEl.value) }), RATE_DEBOUNCE_MS);
    });

    poll();
})();
