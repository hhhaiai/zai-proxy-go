const puppeteer = require('puppeteer-core');
const http = require('http');
const crypto = require('crypto');
const uuidv4 = () => crypto.randomUUID();

const CHROME_PATH = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const ZAI_URL = 'https://chat.z.ai';
const PORT = parseInt(process.env.BROWSER_PROXY_PORT || '9877');
const POOL_SIZE = parseInt(process.env.BROWSER_POOL_SIZE || '5');

let browser;
let initialized = false;

// Worker pool
const workers = [];      // all workers
const readyQueue = [];    // available workers (resolve callbacks)
const requestQueue = [];  // pending requests (resolve callbacks)

async function launchBrowser() {
    console.log('[*] Launching browser...');
    browser = await puppeteer.launch({
        executablePath: CHROME_PATH,
        headless: 'new',
        args: [
            '--no-sandbox',
            '--disable-setuid-sandbox',
            '--disable-dev-shm-usage',
            '--disable-gpu',
            '--window-size=1280,720',
            '--disable-blink-features=AutomationControlled',
            '--disable-extensions',
            '--disable-background-networking',
        ]
    });
    console.log('[+] Browser launched');
}

// Create a single worker (page + CDP session)
async function createWorker(id) {
    const page = await browser.newPage();
    await page.setUserAgent('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36');
    page.setDefaultTimeout(180000);

    const cdp = await page.target().createCDPSession();

    // Intercept chat completions responses AND captcha responses
    await cdp.send('Fetch.enable', {
        patterns: [
            { urlPattern: '*api/v2/chat/completions*', requestStage: 'Response' },
            { urlPattern: '*VerifyCaptcha*', requestStage: 'Response' },
        ]
    });

    const worker = {
        id,
        page,
        cdp,
        busy: false,
        lastUsed: 0,
        requestCount: 0,
    };

    // CDP response handler - set fresh for each request
    let resolveChat = null;
    let chatChunks = [];

    cdp.on('Fetch.requestPaused', async (params) => {
        const url = params.request.url;
        try {
            if (url.includes('api/v2/chat/completions')) {
                const resp = await cdp.send('Fetch.getResponseBody', { requestId: params.requestId });
                let text = resp.body;
                if (resp.base64Encoded) text = Buffer.from(text, 'base64').toString('utf-8');

                const status = params.responseStatusCode;
                if (resolveChat) {
                    resolveChat({ status, body: text });
                    resolveChat = null;
                }
            }

            if (url.includes('VerifyCaptcha')) {
                try {
                    const resp = await cdp.send('Fetch.getResponseBody', { requestId: params.requestId });
                    let text = resp.body;
                    if (resp.base64Encoded) text = Buffer.from(text, 'base64').toString('utf-8');
                    const json = JSON.parse(text);
                    if (json.Result && json.Result.securityToken) {
                        console.log(`[W${id}] Captcha solved (TRACELESS)`);
                    }
                } catch(e) {}
            }
        } catch (e) {
            console.log(`[W${id}] CDP error: ${e.message}`);
        }

        try {
            await cdp.send('Fetch.continueRequest', { requestId: params.requestId });
        } catch (e) {}
    });

    // Set up the chat response promise for a request
    worker.setChatHandler = () => {
        chatChunks = [];
        return new Promise((resolve, reject) => {
            resolveChat = resolve;
            setTimeout(() => {
                if (resolveChat === resolve) {
                    resolveChat = null;
                    reject(new Error('Chat response timeout'));
                }
            }, 180000);
        });
    };

    // Load chat.z.ai
    console.log(`[W${id}] Loading chat.z.ai...`);
    await page.goto(ZAI_URL, { waitUntil: 'networkidle2', timeout: 60000 });
    await new Promise(r => setTimeout(r, 2000));
    console.log(`[W${id}] Ready`);

    return worker;
}

// Navigate worker to a fresh conversation
async function resetWorker(worker) {
    try {
        const newChatId = uuidv4().replace(/-/g, '').substring(0, 24);
        await worker.page.goto(`${ZAI_URL}/c/${newChatId}`, { waitUntil: 'networkidle2', timeout: 30000 });
        await new Promise(r => setTimeout(r, 1000));
    } catch (e) {
        console.log(`[W${worker.id}] Reset failed, reloading: ${e.message}`);
        try {
            await worker.page.goto(ZAI_URL, { waitUntil: 'networkidle2', timeout: 30000 });
            await new Promise(r => setTimeout(r, 1000));
        } catch (e2) {
            console.log(`[W${worker.id}] Reload also failed: ${e2.message}`);
        }
    }
}

// Initialize pool
async function init() {
    await launchBrowser();

    console.log(`[*] Creating pool of ${POOL_SIZE} workers...`);
    const promises = [];
    for (let i = 0; i < POOL_SIZE; i++) {
        promises.push(createWorker(i).then(w => {
            workers.push(w);
            // Mark as ready
            readyQueue.push(w);
        }));
    }
    await Promise.all(promises);

    initialized = true;
    console.log(`[+] Browser proxy ready with ${POOL_SIZE} workers`);
}

// Get an available worker (wait if all busy)
function getWorker() {
    return new Promise((resolve) => {
        // Try to find a non-busy worker
        for (const w of workers) {
            if (!w.busy) {
                w.busy = true;
                resolve(w);
                return;
            }
        }
        // All busy - queue the request
        requestQueue.push(resolve);
    });
}

// Release a worker back to the pool
function releaseWorker(worker) {
    worker.busy = false;
    worker.lastUsed = Date.now();

    // Check if any queued requests are waiting
    if (requestQueue.length > 0) {
        const next = requestQueue.shift();
        worker.busy = true;
        next(worker);
    }
}

// Recover a failed worker
async function recoverWorker(worker) {
    console.log(`[W${worker.id}] Recovering...`);
    try {
        worker.page.removeAllListeners();
        await worker.page.close().catch(() => {});
    } catch (e) {}

    try {
        const newWorker = await createWorker(worker.id);
        // Replace in workers array
        const idx = workers.indexOf(worker);
        if (idx >= 0) workers[idx] = newWorker;
        newWorker.busy = false;
        console.log(`[W${worker.id}] Recovered successfully`);
        return newWorker;
    } catch (e) {
        console.log(`[W${worker.id}] Recovery failed: ${e.message}`);
        return null;
    }
}

// Parse SSE response from z.ai upstream
function parseUpstreamResponse(body) {
    const lines = body.split('\n');
    let fullContent = '';
    let fullReasoning = '';
    let hasError = false;
    let errorMsg = '';
    let answerEditContent = '';
    let totalContentLen = 0;

    for (const line of lines) {
        if (!line.startsWith('data: ')) continue;
        const payload = line.substring(6).trim();
        if (payload === '[DONE]') break;

        try {
            const data = JSON.parse(payload);
            const d = data.data || {};

            const err = d.error || d.data?.error;
            if (err) {
                hasError = true;
                errorMsg = err.detail || err.code || 'Unknown error';
                break;
            }

            if (d.phase === 'done') break;

            if (d.phase === 'thinking' && d.delta_content) {
                fullReasoning += d.delta_content;
            }

            if (d.phase === 'answer') {
                if (d.edit_content) {
                    answerEditContent = d.edit_content;
                } else if (d.delta_content) {
                    fullContent += d.delta_content;
                }
            }

            if ((d.phase === 'other' || d.phase === 'tool_call') && d.edit_content) {
                const fullRunes = [...d.edit_content];
                if (fullRunes.length > totalContentLen) {
                    fullContent = fullRunes.slice(totalContentLen).join('');
                    totalContentLen = fullRunes.length;
                }
            }
        } catch (e) {}
    }

    if (answerEditContent) {
        const detailsEnd = answerEditContent.indexOf('</details>');
        if (detailsEnd !== -1) {
            let afterDetails = answerEditContent.substring(detailsEnd + '</details>'.length);
            if (afterDetails.startsWith('\n')) afterDetails = afterDetails.substring(1);
            fullContent = afterDetails;
        } else {
            fullContent = answerEditContent;
        }
    }

    return { content: fullContent || fullReasoning, hasError, errorMsg };
}

// Handle a single chat request on a worker
async function processChat(worker, chatReq) {
    const prompt = chatReq.messages?.[chatReq.messages.length - 1]?.content || '';
    console.log(`[W${worker.id}] Processing: ${prompt.substring(0, 60)}...`);
    worker.requestCount++;

    // Navigate to a fresh conversation for clean state
    await resetWorker(worker);

    // Set up CDP response capture BEFORE sending
    const responsePromise = worker.setChatHandler();

    // Type the message
    const textarea = await worker.page.$('textarea');
    if (!textarea) {
        throw new Error('No textarea found on page');
    }

    await worker.page.evaluate((text) => {
        const ta = document.querySelector('textarea');
        if (ta) {
            const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
            setter.call(ta, text);
            ta.dispatchEvent(new Event('input', { bubbles: true }));
            ta.dispatchEvent(new Event('change', { bubbles: true }));
        }
    }, prompt);

    await new Promise(r => setTimeout(r, 200));

    // Press Enter to send
    await worker.page.keyboard.press('Enter');
    console.log(`[W${worker.id}] Sent, waiting for response...`);

    // Wait for CDP-captured response
    const chatResponse = await responsePromise;

    if (!chatResponse || !chatResponse.body) {
        throw new Error('Empty response from upstream');
    }

    console.log(`[W${worker.id}] Got response: status=${chatResponse.status}, ${chatResponse.body.length} bytes`);

    // Parse the SSE response
    const parsed = parseUpstreamResponse(chatResponse.body);

    if (parsed.hasError) {
        throw new Error(parsed.errorMsg);
    }

    if (!parsed.content) {
        throw new Error('No content in response');
    }

    console.log(`[W${worker.id}] Response: ${parsed.content.substring(0, 60)}...`);
    return parsed.content;
}

// HTTP handler for chat completions
async function handleChat(req, res) {
    if (!initialized) {
        res.writeHead(503, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: { message: 'Browser proxy not ready', type: 'server_error' } }));
        return;
    }

    let body = '';
    req.on('data', c => body += c);
    req.on('end', async () => {
        let worker = null;
        try {
            const chatReq = JSON.parse(body);
            const stream = chatReq.stream !== false;

            // Get a worker from the pool
            worker = await getWorker();

            let responseText;
            try {
                responseText = await processChat(worker, chatReq);
            } catch (e) {
                console.log(`[W${worker.id}] Error: ${e.message}, recovering...`);
                // Try to recover the worker
                const recovered = await recoverWorker(worker);
                if (recovered) {
                    worker = recovered;
                    // Retry once with recovered worker
                    worker.busy = true;
                    responseText = await processChat(worker, chatReq);
                } else {
                    throw new Error('Worker recovery failed');
                }
            }

            // Return in OpenAI format
            if (stream) {
                res.writeHead(200, {
                    'Content-Type': 'text/event-stream',
                    'Cache-Control': 'no-cache',
                    'Connection': 'keep-alive',
                    'Access-Control-Allow-Origin': '*',
                });

                const chunk = {
                    id: 'chatcmpl-' + Date.now().toString(36),
                    object: 'chat.completion.chunk',
                    created: Math.floor(Date.now() / 1000),
                    model: chatReq.model || 'glm-4.7',
                    choices: [{
                        index: 0,
                        delta: { role: 'assistant', content: responseText },
                        finish_reason: null,
                    }],
                };
                res.write(`data: ${JSON.stringify(chunk)}\n\n`);

                const doneChunk = {
                    id: chunk.id,
                    object: 'chat.completion.chunk',
                    created: chunk.created,
                    model: chunk.model,
                    choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
                };
                res.write(`data: ${JSON.stringify(doneChunk)}\n\n`);
                res.write('data: [DONE]\n\n');
                res.end();
            } else {
                res.writeHead(200, {
                    'Content-Type': 'application/json',
                    'Access-Control-Allow-Origin': '*',
                });
                res.end(JSON.stringify({
                    id: 'chatcmpl-' + Date.now().toString(36),
                    object: 'chat.completion',
                    created: Math.floor(Date.now() / 1000),
                    model: chatReq.model || 'glm-4.7',
                    choices: [{
                        index: 0,
                        message: { role: 'assistant', content: responseText },
                        finish_reason: 'stop',
                    }],
                    usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
                }));
            }

        } catch (e) {
            console.log(`[-] Error: ${e.message}`);
            if (!res.headersSent) {
                res.writeHead(500, { 'Content-Type': 'application/json' });
            }
            res.end(JSON.stringify({ error: { message: e.message } }));
        } finally {
            if (worker) {
                releaseWorker(worker);
            }
        }
    });
}

function handleModels(req, res) {
    res.writeHead(200, {
        'Content-Type': 'application/json',
        'Access-Control-Allow-Origin': '*',
    });
    res.end(JSON.stringify({
        data: [
            { id: 'glm-4.7', object: 'model', created: Date.now(), owned_by: 'zai' },
        ]
    }));
}

function handleStatus(req, res) {
    const workerStatus = workers.map(w => ({
        id: w.id,
        busy: w.busy,
        requests: w.requestCount,
    }));

    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
        initialized,
        pool_size: workers.length,
        busy_count: workers.filter(w => w.busy).length,
        queue_length: requestQueue.length,
        workers: workerStatus,
    }));
}

function handleCORS(req, res) {
    res.writeHead(200, {
        'Access-Control-Allow-Origin': '*',
        'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
        'Access-Control-Allow-Headers': 'Content-Type, Authorization',
    });
    res.end();
}

const server = http.createServer((req, res) => {
    if (req.method === 'OPTIONS') {
        handleCORS(req, res);
        return;
    }
    const path = req.url.split('?')[0];
    if (path === '/v1/chat/completions' || path === '/chat/completions') {
        handleChat(req, res);
    } else if (path === '/v1/models' || path === '/models') {
        handleModels(req, res);
    } else if (path === '/status') {
        handleStatus(req, res);
    } else {
        res.writeHead(404);
        res.end('Not Found');
    }
});

server.listen(PORT, async () => {
    console.log(`[*] Browser proxy listening on port ${PORT} (pool size: ${POOL_SIZE})`);
    try {
        await init();
    } catch (e) {
        console.error(`[-] Init failed: ${e.message}`);
    }
});
