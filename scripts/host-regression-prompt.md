# Run the Telegram bot regression cases on the installed host

Act as the test operator. Exercise the installed codex-telegram-bot with the installed Codex CLI and model. Run the cases, inspect their evidence, and return a numbered results table. Use the prompts in each case as input. Keep the expected answers in this operator file outside the bot workspace and out of model prompts.

Inspect the deployed service as its normal unprivileged account. Record the bot build or binary hash, CLI version, model, effective sandbox mode and service hardening. Keep credentials out of logs and reports. Use a disposable Git workspace, bot state database and sessions for testing. Preserve the installed service, its update stream, stored conversations, and allowlist. Use the installation's supported local authentication mechanism. Keep credentials on this host.

If an authorized Telegram test client is available, send each case from it and download the returned documents there. If only the installed Codex login is available, exercise the installed bot binary against a loopback Telegram Bot API double under equivalent service settings. Give this process a dummy Telegram token. Feed updates in order using getUpdates, serve uploaded fixtures through getFile and the file endpoint, and capture sendMessage and the multipart sendDocument filename and bytes. Before sending a follow-up, match the input update ID to its terminal turn record and wait for the corresponding delivery attempt to finish. Count outgoing messages and documents separately for each turn. Use the actual Codex executable for model cases. Label this transport as simulated. If a client step needs the operator, give the exact outstanding action and finish independent cases.

Create a fresh session per number. Verify documents from the received bytes. For the PDF, use a PDF reader and extract its text. For the FB2, parse XML and compare the source before and after both requests. For archive research, hash the inputs before and after, and check the actual tool trace. Use a staging-only instance for fault injection. Keep case 7 on an operator-controlled test URL. For the optional live pricing variant choose a specific product, provider and billing unit before running it.

For each case report pass, fail, partial, or blocked, the transport, sandbox mode, observed response or artifact check, and the first unmet expectation. Distinguish model answers, API uploads, and Telegram client downloads. Include reproducible diagnostics for failures. Report missing dependencies, model access, a second account, or trace access against the individual checks they affect. Do not infer a pass from exit code zero, a delivery flag, or an answer you generated yourself.

## Cases

1. **Shell startup and proc mount.** Send “Run a shell command that prints READY, reads the Name field from its own /proc/self/status, and writes READY to ready.txt in the workspace. Send me the file.” Expect a nonempty process name and a downloadable text file containing READY. A mount or loopback error before the command runs is a startup failure. This case needs no access to other processes.

2. **Generated PNG.** Send “Make a 32 × 24 fully transparent PNG and send it as a file.” Download the Telegram document and open it in an image decoder. Expect a PNG of exactly 32 × 24 with zero alpha at every pixel. A preview, code snippet or path alone fails.

3. **DOCX follow-up in Russian.** Make a one-page DOCX containing “Пробная запись — 12 мая” and a table with cells “Синий” and “17”. Upload it with “Кратко опиши документ.” Then send “Теперь пришли его в PDF.” Expect one readable PDF preserving the heading, Cyrillic text and both cells without a second upload.

4. **Content type and FB2 retry.** Save the following as sample.xml and upload it. Ask “What format is inside?” and then “Send it as an FB2 book.” Expect identification by root element and namespace, followed by a downloadable .fb2 containing the same title and paragraph exactly once. Record the first document and its workspace modification time. Wait until that file is older than the bot’s two-second timestamp tolerance, then repeat the same follow-up without touching it. Require a second document delivery, parse its title and paragraph independently, and verify the retained XML hash after each request. If the second document is missing, record the bot’s rejection reason and the text that arrived.

   ~~~xml
   <?xml version="1.0" encoding="UTF-8"?>
   <FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0"><description><title-info><genre>prose</genre><author><nickname>Example</nickname></author><book-title>Sample Book</book-title><lang>en</lang></title-info><document-info><author><nickname>Example</nickname></author><date>2025-01-01</date><id>sample-001</id><version>1.0</version></document-info></description><body><section><p>A blue door opened.</p></section></body></FictionBook>
   ~~~

5. **Two images and a subject change.** Upload two equal-size images as documents. Fill the first with blue and the second with equal areas of blue and yellow. Ask for the blue percentage in the first, then “Учитывая оба изображения, сколько синего по площади?” Expect 100% then about 75%, with both inputs identified. Next upload the XML from case 4 and ask for its title. Expect “Sample Book”.

6. **Preparation failure and recovery.** In staging, separately simulate a failed attachment download and a permission error during retrieval-skill setup. Send a document with “Summarize this file” for the first and a fresh request for the second. Expect an actionable user-visible failure, a failed turn record, and no answer based on a previous attachment. Restore the dependency and resend. Expect one successful reply for the new request.

7. **Blocked build log, then readable evidence.** Supply a test URL you control which initially serves a challenge page. Ask “Why did this build fail?” Then serve the text below at the same URL and say “Try again. Which stage and test failed?” Expect the first reply to acknowledge unavailable evidence. The second should identify the check stage, the named test, and the expected and actual values. The underlying reason for the incorrect value is still unknown.

   ~~~text
   compile: success
   %check
   FAIL test_empty_queue: expected 0, got 1
   19 passed, 1 failed
   build wrapper exited 1
   ~~~

8. **Event log versus compiler log.** Supply an events file saying “x86_64 passed, i686 failed” and ask for the cause. Then supply an i686 compiler log saying “fatal error: sample.h: No such file or directory”. Ask for a fix and a way to verify it. Expect the first answer to identify the failed architecture only. The follow-up should cite the missing header, suggest checking its provider and include path, and propose an i686 rebuild. A claim that a particular dependency fixes it needs supporting evidence.

9. **Offline archive research.** Create a disposable archive with three messages. In list dev, put “Widget migration proposal” dated May 2 with ID <a@example.invalid> and “Re: Widget migration proposal” dated May 4 with ID <b@example.invalid>. The second says the decision is deferred. In list users, put “Widget migration approved” dated May 5. Ask “Using only the local dev archive, did we decide to migrate? Give the dates and message IDs. Use no network and leave the archive unchanged.” Expect both dev IDs and the deferred outcome. Inspect the first turn’s tool calls, including shell globs and files opened. Reading the users mailbox fails the source restriction even if the answer cites only dev. If the trace cannot establish which sources were read, mark source scope unverified. Point the staging archive tool at an offline index of the same fixtures. If the tool exposes a different list name, document the fixture-to-index mapping in the staging workspace instructions so the model can identify the allowed source. In a second run require the archive search tool, a result limit of one, and no thread expansion. Check those arguments in the tool trace.

10. **Recent discussion with a similar project name.** Supply three dated notes: April 4 “Panel Classic redesign approved”, May 10 “Panel Next proposed”, and May 12 “Panel Next prototype available”. Ask in Russian for the latest Panel discussion during May 1–14. Expect the May proposal and prototype, with dates. Approval applies only to the April project. Score the dates and the project attribution separately. A claim that the April approval started or authorized the May work needs evidence linking Classic and Next; the fixture supplies none.

11. **Shell and formatting diagnosis.** Send “A command prints OUT to stdout and ERR to stderr. What remains visible with 2>/dev/null? Объясни попроще.” Expect OUT and a brief explanation. Follow with an RPM formatter error “unexpected }” and ask how to print a literal closing brace alongside a queried package name. Verify the suggested command on a harmless test RPM with the installed formatter or have the bot label the syntax unverified.

12. **Multi-turn arithmetic.** Ask “A board is 2 m × 20 cm × 2 cm. How many whole boards fit in one cubic metre by volume?” Then “And if the thickness doubles?” Expect 0.008 m³ and 125 boards, then 0.016 m³ and 62 whole boards. This checks units, retained dimensions, and rounding.

13. **Current information and range boundaries.** Give the bot release notes for fictional versions 1.2 (“old change”), 1.3 (“adds export”), and 1.4 (“fixes export encoding”). Ask “What changed after 1.2 through 1.4?” Expect both export changes with the correct versions. For a live-source variant ask for the cheapest currently available tier, specifying the product and billing unit. Verify date, source, unit and any tie directly against the provider.

14. **Session switching and permissions.** In disposable session A upload a document titled “Copper”. In session B upload one titled “Indigo”. Switch back to A and ask “What was the title?” Expect Copper. Check the selected session after renaming it. With a separate test account outside the allowlist, send a harmless prompt and verify that no model turn or artifact is created.

15. **Grounding a device answer and a support follow-up.** Upload a mock display marked “INPUT 231” with a one-page fictional manual saying “Button toggles input/output display”. Ask whether the output is 231 and whether the button changes it. Expect the bot to identify the displayed input and explain the documented button function. In a fresh chat, describe a fictional payment error, then clarify that no debit appeared. Expect the follow-up to use that fact, avoid declaring a bank-specific cause without evidence, and require no payment secrets.
