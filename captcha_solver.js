const puppeteer = require('puppeteer-core');

const CHROME_PATH = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const ZAI_URL = 'https://chat.z.ai';
const USE_HEADLESS = process.argv.includes('--no-headless') ? false : 'new';

async function solveCaptcha() {
    let browser;
    try {
        browser = await puppeteer.launch({
            executablePath: CHROME_PATH,
            headless: USE_HEADLESS,
            args: [
                '--no-sandbox',
                '--disable-setuid-sandbox',
                '--disable-dev-shm-usage',
                '--disable-gpu',
                '--window-size=1280,720',
                '--disable-blink-features=AutomationControlled',
            ]
        });

        const page = await browser.newPage();
        await page.setUserAgent('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36');

        // CDP Fetch interception
        const client = await page.target().createCDPSession();

        let securityToken = '';
        let certifyId = '';
        let verifyResponse = null;

        await client.send('Fetch.enable', {
            patterns: [
                { urlPattern: '*captcha-open*', requestStage: 'Response' },
                { urlPattern: '*VerifyCaptcha*', requestStage: 'Response' },
                { urlPattern: '*InitCaptcha*', requestStage: 'Response' },
            ]
        });

        client.on('Fetch.requestPaused', async (params) => {
            const url = params.request.url;
            try {
                const response = await client.send('Fetch.getResponseBody', {
                    requestId: params.requestId
                });

                let text = response.body;
                if (response.base64Encoded) {
                    text = Buffer.from(text, 'base64').toString('utf-8');
                }

                try {
                    const json = JSON.parse(text);
                    if (json.Result && json.Result.securityToken) {
                        securityToken = json.Result.securityToken;
                        certifyId = json.Result.certifyId || '';
                        verifyResponse = json;
                        process.stderr.write(`[FETCH] *** Got securityToken! certifyId=${certifyId} ***\n`);
                    }
                    if (json.CertifyId) {
                        certifyId = json.CertifyId;
                    }
                } catch(e) {}
            } catch (e) {}

            try {
                await client.send('Fetch.continueRequest', { requestId: params.requestId });
            } catch (e) {}
        });

        // Step 1: Load chat.z.ai
        process.stderr.write('[*] Loading chat.z.ai...\n');
        await page.goto(ZAI_URL, { waitUntil: 'networkidle2', timeout: 60000 });
        process.stderr.write('[+] Page loaded\n');

        // Step 2: Wait
        await new Promise(r => setTimeout(r, 5000));

        // Step 3: Get token
        const token = await page.evaluate(() => localStorage.getItem('token') || '');
        process.stderr.write(`[+] Token: ${token ? 'yes' : 'none'}\n`);

        // Step 4: Intercept the fetch API to capture the captcha_verify_param
        // that the browser builds, and also intercept the chat request
        await page.evaluate(() => {
            window.__captchaVerifyParam = '';
            window.__chatRequestCaptured = false;

            const origFetch = window.fetch;
            window.fetch = async function(...args) {
                const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';

                // Intercept chat completion requests
                if (url.includes('/api/v2/chat/completions')) {
                    try {
                        const body = args[1]?.body;
                        if (body) {
                            const parsed = JSON.parse(body);
                            if (parsed.captcha_verify_param) {
                                window.__captchaVerifyParam = parsed.captcha_verify_param;
                                console.log('[INTERCEPT] Captured captcha_verify_param');
                            }
                        }
                    } catch(e) {}
                }

                return origFetch.apply(this, args);
            };
        });

        // Step 5: Type and send to trigger captcha
        process.stderr.write('[*] Typing and sending message...\n');
        const textarea = await page.$('textarea');
        if (textarea) {
            await textarea.click();
            await textarea.type('test', { delay: 100 });
            await new Promise(r => setTimeout(r, 500));
            await page.keyboard.press('Enter');
            process.stderr.write('[*] Message sent\n');
        }

        // Step 6: Wait for captcha
        process.stderr.write('[*] Waiting for captcha (45s)...\n');
        await new Promise(r => setTimeout(r, 45000));

        // Step 7: Get the captured captcha_verify_param from the browser
        const capturedParam = await page.evaluate(() => window.__captchaVerifyParam || '');
        process.stderr.write(`[+] Captured param from browser: ${capturedParam ? capturedParam.substring(0, 30) + '...' : 'none'}\n`);

        // Step 8: Also build from CDP-captured securityToken
        let cdpParam = '';
        if (securityToken) {
            const verifyData = {
                certifyId: certifyId,
                sceneId: 'didk33e0',
                isSign: true,
                securityToken: securityToken
            };
            cdpParam = Buffer.from(JSON.stringify(verifyData)).toString('base64');
            process.stderr.write(`[+] CDP param: ${cdpParam.substring(0, 30)}...\n`);
        }

        // Use whichever param we got
        const captchaVerifyParam = capturedParam || cdpParam;

        // Step 9: Get cookies
        const cookies = await page.cookies();
        const cookieStr = cookies.map(c => `${c.name}=${c.value}`).join('; ');

        // Output
        const result = {
            token: token,
            securityToken: securityToken,
            certifyId: certifyId,
            captchaVerifyParam: captchaVerifyParam,
            cookies: cookieStr,
            hasToken: !!token,
            hasSecurityToken: !!securityToken,
            hasCaptchaParam: !!captchaVerifyParam,
            source: capturedParam ? 'browser-intercept' : (cdpParam ? 'cdp' : 'none'),
        };

        process.stderr.write(`[+] Result: token=${!!token}, securityToken=${!!securityToken}, captchaParam=${!!captchaVerifyParam}\n`);
        process.stdout.write(JSON.stringify(result) + '\n');

    } catch (error) {
        process.stderr.write(`[ERROR] ${error.message}\n`);
        process.stdout.write(JSON.stringify({ error: error.message }) + '\n');
        process.exit(1);
    } finally {
        if (browser) await browser.close();
    }
}

solveCaptcha();
