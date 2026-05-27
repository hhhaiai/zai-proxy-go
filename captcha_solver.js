const puppeteer = require('puppeteer-core');

const CHROME_PATH = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const ZAI_URL = 'https://chat.z.ai';

const MESSAGE = process.argv.find(a => a.startsWith('--msg='))?.slice(6) || '';

async function solveCaptcha() {
    let browser;
    try {
        browser = await puppeteer.launch({
            executablePath: CHROME_PATH,
            headless: 'new',
            args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage', '--disable-gpu',
                   '--window-size=1280,720', '--disable-blink-features=AutomationControlled'],
        });

        const page = await browser.newPage();
        await page.setUserAgent('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.000 Safari/537.36');

        // Load page
        process.stderr.write('[*] Loading...\n');
        await page.goto(ZAI_URL, { waitUntil: 'networkidle2', timeout: 60000 });
        await new Promise(r => setTimeout(r, 3000));

        const token = await page.evaluate(() => localStorage.getItem('token') || '');
        const cookies = await page.cookies();
        const cookieStr = cookies.map(c => `${c.name}=${c.value}`).join('; ');

        if (!MESSAGE) {
            process.stdout.write(JSON.stringify({
                token, securityToken: '', certifyId: '', captchaVerifyParam: '',
                cookies: cookieStr, hasToken: !!token, hasSecurityToken: false,
                hasCaptchaParam: false, source: 'browser-session'
            }) + '\n');
            return;
        }

        // Send message
        process.stderr.write(`[*] Sending: "${MESSAGE}"\n`);
        const textarea = await page.$('#chat-input') || await page.$('textarea');
        if (!textarea) throw new Error('No textarea');

        await textarea.click();
        await textarea.type(MESSAGE, { delay: 30 });
        await new Promise(r => setTimeout(r, 300));
        await page.keyboard.press('Enter');
        process.stderr.write('[*] Sent, polling...\n');

        // Poll for response completion (page text stabilizes)
        let prevLen = 0;
        let stableCount = 0;
        for (let i = 0; i < 240; i++) { // 120s max
            await new Promise(r => setTimeout(r, 500));
            const len = await page.evaluate(() => document.body.innerText.length);
            if (len === prevLen) {
                stableCount++;
                if (stableCount >= 8) break; // 4 seconds stable
            } else {
                stableCount = 0;
            }
            prevLen = len;
        }

        // Extract: get all text, find content between user message and disclaimer
        const answer = await page.evaluate((userMsg) => {
            const body = document.body.innerText;
            const lines = body.split('\n').map(l => l.trim()).filter(l => l);

            // Find the last occurrence of the user message
            let lastUserIdx = -1;
            for (let i = lines.length - 1; i >= 0; i--) {
                if (lines[i] === userMsg || lines[i].includes(userMsg)) {
                    lastUserIdx = i;
                    break;
                }
            }

            if (lastUserIdx === -1) return '';

            // Collect lines after user message until footer
            const answerLines = [];
            for (let i = lastUserIdx + 1; i < lines.length; i++) {
                const line = lines[i];
                const lower = line.toLowerCase();
                // Stop at footer/disclaimer
                if (line.includes('以上内容均由AI生成')) break;
                if (line.includes('技术博客')) break;
                if (line.includes('联系我们')) break;
                if (line.includes('用户协议')) break;
                if (line.includes('隐私政策')) break;
                if (lower.includes('有什么我能帮')) break;
                if (lower === 'new chat') break;
                if (line.includes('Regenerate') || line.includes('Copy') || line.includes('Like') || line.includes('Dislike')) {
                    // UI buttons, skip
                    continue;
                }
                answerLines.push(line);
            }

            return answerLines.join('\n').trim();
        }, MESSAGE);

        // Clean up: the actual answer is the last line/paragraph (thinking comes before it)
        let cleanAnswer = answer;
        const lines = answer.split('\n').filter(l => l.trim());
        if (lines.length > 1) {
            // Last line is typically the actual response
            cleanAnswer = lines[lines.length - 1].trim();
        }

        process.stderr.write(`[+] Answer: ${cleanAnswer.length} chars (raw: ${answer.length})\n`);
        process.stdout.write(JSON.stringify({
            token, answer: cleanAnswer, cookies: cookieStr,
            hasToken: !!token, hasAnswer: !!cleanAnswer, source: 'page-extract'
        }) + '\n');

    } catch (error) {
        process.stderr.write(`[ERROR] ${error.message}\n`);
        process.stdout.write(JSON.stringify({ error: error.message }) + '\n');
        process.exit(1);
    } finally {
        if (browser) await browser.close();
    }
}

solveCaptcha();
