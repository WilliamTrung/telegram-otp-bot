// App State Management
let activePollInterval = null;
let activeCountdownInterval = null;
let currentToken = null;

// Tab Routing System
function switchTab(tabId) {
    // Hide all tab panes
    document.querySelectorAll('.tab-pane').forEach(pane => {
        pane.classList.remove('active');
    });

    // Remove active state from all nav buttons
    document.querySelectorAll('.nav-btn').forEach(btn => {
        btn.classList.remove('active');
    });

    // Show active pane and button
    document.getElementById(tabId).classList.add('active');
    
    // Update active nav button styling
    if (tabId === 'tab-register') {
        document.getElementById('nav-btn-register').classList.add('active');
        document.getElementById('page-title').innerText = 'Developer Console';
    } else if (tabId === 'tab-sandbox') {
        document.getElementById('nav-btn-sandbox').classList.add('active');
        document.getElementById('page-title').innerText = 'Verification Sandbox';
    } else if (tabId === 'tab-docs') {
        document.getElementById('nav-btn-docs').classList.add('active');
        document.getElementById('page-title').innerText = 'Documentation';
    }
}

// Clipboard copying utility
function copyToClipboard(elementId, btnElement) {
    const text = document.getElementById(elementId).innerText;
    navigator.clipboard.writeText(text).then(() => {
        const icon = btnElement.querySelector('i');
        icon.className = 'fa-solid fa-check';
        icon.style.color = '#10b981';
        
        setTimeout(() => {
            icon.className = 'fa-regular fa-copy';
            icon.style.color = '';
        }, 1500);
    }).catch(err => {
        console.error('Failed to copy text: ', err);
    });
}

// Client API key registration
function registerClient() {
    const nameInput = document.getElementById('client-name');
    const name = nameInput.value.trim();

    if (!name) {
        alert('Please enter an Application Name.');
        return;
    }

    const registerBtn = document.getElementById('btn-register');
    registerBtn.disabled = true;
    registerBtn.innerHTML = '<i class="fa-solid fa-spinner fa-spin"></i> Generating...';

    fetch('/api/clients', {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify({ name: name })
    })
    .then(response => {
        if (!response.ok) {
            throw new Error('Registration failed with status: ' + response.status);
        }
        return response.json();
    })
    .then(data => {
        // Display credentials card
        document.getElementById('display-client-id').innerText = data.id;
        document.getElementById('display-api-key').innerText = data.api_key;
        document.getElementById('credentials-card').classList.remove('hidden');

        // Automatically populate Sandbox fields for ease of testing
        document.getElementById('sandbox-api-key').value = data.api_key;

        // Clear input field
        nameInput.value = '';
    })
    .catch(err => {
        console.error(err);
        alert('Error generating credentials. Ensure the Go backend API is running.');
    })
    .finally(() => {
        registerBtn.disabled = false;
        registerBtn.innerHTML = '<i class="fa-solid fa-wand-magic-sparkles"></i> Generate Credentials';
    });
}

// Initiate verification flow in Sandbox
function initiateVerification() {
    // Clear any previous interval/timers
    clearInterval(activePollInterval);
    clearInterval(activeCountdownInterval);

    const apiKey = document.getElementById('sandbox-api-key').value.trim();
    const callbackURL = document.getElementById('sandbox-callback').value.trim() || 'http://localhost:9090/webhook';
    const userRef = document.getElementById('sandbox-user-ref').value.trim() || 'user_sandbox_99';

    if (!apiKey) {
        alert('Please register a client first or input an API Key to authenticate the request.');
        return;
    }

    // Prepare sandbox logging pane
    document.getElementById('console-card').classList.remove('hidden');
    clearLogs();
    logMessage('Initiating verification session request to /api/verify/init...', 'info');

    const initBtn = document.getElementById('btn-initiate');
    initBtn.disabled = true;
    initBtn.innerHTML = '<i class="fa-solid fa-spinner fa-spin"></i> Initiating...';

    fetch('/api/verify/init', {
        method: 'POST',
        headers: {
            'Authorization': 'Bearer ' + apiKey,
            'Content-Type': 'application/json'
        },
        body: JSON.stringify({
            callback_url: callbackURL,
            user_reference: userRef
        })
    })
    .then(response => {
        if (response.status === 401) {
            throw new Error('Unauthorized: Invalid API Key');
        }
        if (!response.ok) {
            throw new Error('HTTP request failed with status: ' + response.status);
        }
        return response.json();
    })
    .then(data => {
        currentToken = data.token;
        logMessage(`Verification session token generated: ${currentToken}`, 'info');
        logMessage(`Deep Link resolved: ${data.telegram_link}`, 'info');

        // Transition status card views
        document.getElementById('status-state-idle').classList.add('hidden');
        document.getElementById('status-state-success').classList.add('hidden');
        document.getElementById('status-state-waiting').classList.remove('hidden');

        // Configure Telegram deep link button & QR code
        document.getElementById('telegram-link-btn').href = data.telegram_link;
        document.getElementById('display-session-token').innerText = currentToken;

        // Render QR Code using a public zero-cost generator service
        const qrImgUrl = `https://api.qrserver.com/v1/create-qr-code/?size=130x130&data=${encodeURIComponent(data.telegram_link)}`;
        document.getElementById('qr-placeholder').innerHTML = `<img src="${qrImgUrl}" alt="Telegram Verification Link QR" style="width: 130px; height: 130px;">`;

        // Start countdown timer (5 mins)
        startCountdown(new Date(data.expires_at));

        // Start polling verification state
        startPolling(currentToken);
    })
    .catch(err => {
        logMessage(`Error: ${err.message}`, 'error');
        alert('Failed to initiate verification session: ' + err.message);
    })
    .finally(() => {
        initBtn.disabled = false;
        initBtn.innerHTML = '<i class="fa-solid fa-rocket"></i> Initiate Verification';
    });
}

// Active polling helper to check backend status
function startPolling(token) {
    logMessage('Starting active status polling loop to check status of token...', 'info');
    
    activePollInterval = setInterval(() => {
        fetch(`/api/verify/status?token=${token}`)
        .then(res => {
            if (!res.ok) throw new Error('Polling check failed');
            return res.json();
        })
        .then(data => {
            logMessage(`Polling status check: ${data.status}`, 'info');

            if (data.status === 'VERIFIED') {
                clearInterval(activePollInterval);
                clearInterval(activeCountdownInterval);
                logMessage('Success! Session is VERIFIED on Telegram.', 'success');
                logMessage(`Telegram User Details: Chat ID = ${data.chat_id}, User = @${data.telegram_user || 'none'}`, 'success');
                logMessage(`Sending webhook notification payload to callback: ${data.callback_url}`, 'warn');

                // Transition status view to Success
                document.getElementById('status-state-waiting').classList.add('hidden');
                document.getElementById('status-state-success').classList.remove('hidden');

                // Populate verified user credentials card
                document.getElementById('success-chat-id').innerText = data.chat_id;
                document.getElementById('success-username').innerText = data.telegram_user ? '@' + data.telegram_user : 'None';
                document.getElementById('success-first-name').innerText = data.telegram_first || 'None';
            } else if (data.status === 'EXPIRED') {
                clearInterval(activePollInterval);
                clearInterval(activeCountdownInterval);
                logMessage('Verification session has expired.', 'warn');
                resetToIdle();
            }
        })
        .catch(err => {
            logMessage(`Polling error: ${err.message}`, 'error');
        });
    }, 1000); // Poll every 1s
}

// 5-minute timer countdown
function startCountdown(expiryTime) {
    activeCountdownInterval = setInterval(() => {
        const diff = expiryTime - new Date();
        if (diff <= 0) {
            clearInterval(activeCountdownInterval);
            document.getElementById('expiry-timer').innerText = '0:00';
            logMessage('Verification timer expired.', 'warn');
            resetToIdle();
            return;
        }

        const minutes = Math.floor(diff / 60000);
        const seconds = Math.floor((diff % 60000) / 1000);
        document.getElementById('expiry-timer').innerText = `${minutes}:${seconds < 10 ? '0' : ''}${seconds}`;
    }, 1000);
}

// Reset sandbox status cards back to default idle screen
function resetToIdle() {
    document.getElementById('status-state-waiting').classList.add('hidden');
    document.getElementById('status-state-success').classList.add('hidden');
    document.getElementById('status-state-idle').classList.remove('hidden');
}

// Terminal Logging Helper functions
function logMessage(text, type = 'info') {
    const consoleLogs = document.getElementById('console-logs');
    if (!consoleLogs) return;

    const timeStr = new Date().toLocaleTimeString();
    const logEl = document.createElement('div');
    logEl.className = `log-entry`;

    let typeClass = 'log-info';
    if (type === 'success') typeClass = 'log-success';
    if (type === 'warn') typeClass = 'log-warn';
    if (type === 'error') typeClass = 'log-error';

    logEl.innerHTML = `<span class="log-time">[${timeStr}]</span> <span class="${typeClass}">${text}</span>`;
    consoleLogs.appendChild(logEl);
    consoleLogs.scrollTop = consoleLogs.scrollHeight; // Auto scroll to bottom
}

function clearLogs() {
    const consoleLogs = document.getElementById('console-logs');
    if (consoleLogs) consoleLogs.innerHTML = '';
}
