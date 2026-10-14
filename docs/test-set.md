# Test set

A small, fixed set of questions to check answer quality. It runs again when the base rules, the tools or the recommended models change. The results are dated, so a model table can point to real numbers.

## How it runs

- Three public repositories, small to large: [urtuba/tc-id](https://github.com/urtuba/tc-id), [urtuba/psytools](https://github.com/urtuba/psytools) and [urtuba/repo-speaks](https://github.com/urtuba/repo-speaks).
- One answer agent per repository. Each reads only its own repository and has no instructions, so only the base rules apply.
- Every question is asked once, by hand, in the web app.
- Each answer is graded **right**, **partly** or **wrong** against the expected answer below. A "not found" question is right only when the answer says so and invents nothing.
- Steps, tokens and seconds come from the stored answer.

The questions are written the way a person who does not read code would ask. Some use other words than the code, some need several files, one is in Turkish, and each repository has one question whose answer is not in the code.

## Questions

| # | Repository | Question | A right answer says | Kind |
| --- | --- | --- | --- | --- |
| 1 | tc-id | Can this tool tell me if an ID number belongs to a real person? | No. It only checks the published checksum. A real check goes through NVİ. | docs and code |
| 2 | tc-id | Why is an ID number that starts with 0 rejected? | The first digit must be 1 to 9. | other words |
| 3 | tc-id | How is the last digit of an ID number worked out? | The sum of the first 10 digits, mod 10. | logic |
| 4 | tc-id | Can I make fake tax numbers for testing? | Yes, `generateVKN`. Valid by the algorithm, for test data only, not registered. | other words |
| 5 | tc-id | Does it handle foreign residents' numbers (starting with 99)? | Nothing treats them differently. | not found |
| 6 | psytools | Danışanlarım hangi testleri Türkçe doldurabilir? | Lists the Turkish packs. Most are unverified AI translations; only ECR-R and HSPS follow published Turkish adaptations. Answers in Turkish. | Turkish |
| 7 | psytools | If a client answers the self-harm question on the depression test, am I warned? | Yes. PHQ-9 item 9 above 0 adds a `suicidality` flag to the result. | other words |
| 8 | psytools | What happens if a client skips some questions? | It depends on the test's missing-data rule: ignore, require complete, or prorate. | several files |
| 9 | psytools | Can a client change answers after sending the form? | No. A submitted response cannot change. | other words |
| 10 | psytools | Does it save my clients' answers on a server? | No storage and no network in the library. | not found |
| 11 | repo-speaks | Can a client see the instructions we gave an agent? | No, only members with `agents:edit` see them. | several files |
| 12 | repo-speaks | How long does an invite link work? | 7 days, once, and it can be revoked. | simple |
| 13 | repo-speaks | What happens if someone types a wrong password many times? | 5 free tries, then waits from 1 second doubling up to 15 minutes. Never locked. | logic |
| 14 | repo-speaks | Our GitHub repository is private. How does the tool read it? | A fine-grained read-only token, sent only to GitHub, never shown again, stored in plain text. | several files |
| 15 | repo-speaks | Can it read our Slack or Notion? | No, GitHub repositories only. | not found |

## Results

### 2026-10-14, `z-ai/glm-5.3-flash`

Commits read: tc-id `b66b0aa`, psytools `808281e`, repo-speaks `425fbbd`.

| # | Grade | Steps | Tokens in | Tokens out | Seconds |
| --- | --- | --- | --- | --- | --- |
| 1 | right | 1 | 5,480 | 722 | 10 |
| 2 | right | 3 | 10,090 | 577 | 9 |
| 3 | right | 2 | 5,966 | 1,296 | 13 |
| 4 | right | 2 | 5,889 | 517 | 7 |
| 5 | right | 6 | 20,389 | 1,457 | 21 |
| 6 | right | 9 | 70,503 | 4,769 | 47 |
| 7 | right | 4 | 38,377 | 1,319 | 17 |
| 8 | right | 13 | 183,381 | 2,606 | 33 |
| 9 | right | 2 | 16,820 | 700 | 9 |
| 10 | right | 6 | 36,382 | 1,279 | 20 |
| 11 | right | 7 | 39,867 | 1,768 | 20 |
| 12 | right | 3 | 21,067 | 509 | 9 |
| 13 | right | 10 | 50,817 | 1,266 | 20 |
| 14 | right | 5 | 28,072 | 1,285 | 17 |
| 15 | right | 6 | 31,154 | 1,350 | 22 |

15 right. Steps: 5.3 on average, 13 at most. Seconds: 18 on average, 47 at most. Tokens: 564,254 in and 21,420 out in total.

Notes:

- Every answer was correct, and the three "not found" answers invented nothing.
- No answer missed a word the code spells another way, such as "self-harm" for `suicidality`.
- The answers were too long and too technical for people who do not read code. They named functions, test files and line numbers.
- Citations stood in the text after every paragraph, and again in the sources list.
