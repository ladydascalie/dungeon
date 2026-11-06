/**
 * dungeon-watch — read the dm session box for lane events.
 *
 * A dungeon lane posts to the session box when it starts, blocks, finishes or
 * fails. The parent does not type into lane panes; it reads the box. This tool
 * gives the parent that read loop: it drains entries past a persisted cursor,
 * then waits for the next one. The agent runs `dungeon next` after a done or
 * error event. The report file and the accept command still decide.
 *
 * The cursor lives in the encounter's `.dungeon/box.cursor`, so a restart
 * resumes where the last read stopped. The first call with no cursor starts at
 * the box's newest entry, so a new encounter does not replay old events.
 */

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const DEFAULT_PREFIX = "lane/";
const DEFAULT_TIMEOUT = 240;
const CURSOR_FILE = "box.cursor";

type Entry = {
	seq: number;
	at: string;
	from: string;
	key: string;
	state: string;
	msg: string;
};

type ListReply = { entries: Entry[]; trimmed: boolean };
type WaitReply = { entry: Entry; trimmed: boolean };

/** Find the encounter directory by walking up for `.dungeon`. */
function encounterDir(cwd: string): string | undefined {
	let dir = path.resolve(cwd);
	for (;;) {
		if (fs.existsSync(path.join(dir, ".dungeon"))) return dir;
		const parent = path.dirname(dir);
		if (parent === dir) return undefined;
		dir = parent;
	}
}

/** The cursor file. Falls back to a cache keyed by cwd outside an encounter. */
function cursorPath(cwd: string): string {
	const enc = encounterDir(cwd);
	if (enc) return path.join(enc, ".dungeon", CURSOR_FILE);
	const key = Buffer.from(path.resolve(cwd)).toString("base64url");
	return path.join(os.homedir(), ".cache", "pi", "dungeon-box", `${key}.cursor`);
}

function readCursor(file: string): number | undefined {
	try {
		const raw = fs.readFileSync(file, "utf8").trim();
		if (raw === "") return undefined;
		const n = Number.parseInt(raw, 10);
		return Number.isFinite(n) && n >= 0 ? n : undefined;
	} catch {
		return undefined;
	}
}

function writeCursor(file: string, seq: number): void {
	fs.mkdirSync(path.dirname(file), { recursive: true });
	fs.writeFileSync(file, `${seq}\n`);
}

/** Parse a box reply. Both list and wait reply shapes are handled. */
function parseReply(stdout: string): ListReply {
	const text = stdout.trim();
	if (text === "") return { entries: [], trimmed: false };
	const reply = JSON.parse(text) as Partial<ListReply & WaitReply>;
	if (reply.entry) return { entries: [reply.entry], trimmed: Boolean(reply.trimmed) };
	return { entries: reply.entries ?? [], trimmed: Boolean(reply.trimmed) };
}

/** dm's CLI defaults to "main", so pass the session the pane is in. */
function dmArgs(args: string[]): string[] {
	const session = process.env.DM_SESSION;
	return session ? [...args, "-s", session] : args;
}

function describe(e: Entry): string {
	const from = e.from ? ` from ${e.from}` : "";
	return `seq ${e.seq}  ${e.key}  ${e.state}${from}  ${e.msg}`;
}

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "dungeon_watch",
		label: "Watch lane box",
		description:
			"Wait for lane events in the dm session box. Returns entries posted by dungeon lanes since the last call. After a done or error event, run `dungeon next` to settle the unit.",
		promptSnippet: "Read the session box for lane events, then run dungeon next.",
		promptGuidelines: [
			"Call dungeon_watch while dungeon lanes are running. It returns lane events past the last read and blocks when nothing is queued.",
			"After a done or error event, run `dungeon next`. After a blocked event, read the lane's pane with `dm read <from>` and answer with `dm write-to <from> --msg \"...\" --enter`.",
			"Do not use `dm msg wait --prefix lane/ --state blocked` to watch a fleet without --after: it reads the newest entry, so a newer done masks an older blocked. This tool uses the cursor.",
		],
		parameters: Type.Object({
			timeout: Type.Optional(
				Type.Number({
					description: `Seconds to block when nothing is queued. Default ${DEFAULT_TIMEOUT}.`,
				}),
			),
			prefix: Type.Optional(Type.String({ description: `Box key prefix. Default "${DEFAULT_PREFIX}".` })),
			state: Type.Optional(
				Type.String({ description: "Only entries in this state: note, working, blocked, done or error." }),
			),
			cursor: Type.Optional(Type.Number({ description: "Start after this seq instead of the persisted cursor." })),
			reset: Type.Optional(
				Type.Boolean({ description: "Ignore the persisted cursor and read from seq 0. Default false." }),
			),
		}),
		async execute(_toolCallId, params, signal, _onUpdate, ctx) {
			const prefix = params.prefix ?? DEFAULT_PREFIX;
			const seconds = Math.max(1, Math.round(params.timeout ?? DEFAULT_TIMEOUT));
			const file = cursorPath(ctx.cwd);

			// An explicit cursor wins. A reset reads from seq 0. Otherwise read the
			// persisted cursor. A first call with neither starts at the newest entry
			// so old events do not replay.
			let cursor = params.cursor;
			if (cursor === undefined) cursor = params.reset ? 0 : readCursor(file);

			const base = ["msg"];
			const filter = ["--prefix", prefix];
			if (params.state) filter.push("--state", params.state);

			if (cursor === undefined) {
				const newest = await pi.exec("dm", dmArgs([...base, "list", ...filter, "--limit", "1", "--json"]), {
					cwd: ctx.cwd,
					signal,
					timeout: 15_000,
				});
				if (newest.code !== 0) {
					return {
						content: [{ type: "text" as const, text: `dm msg list failed: ${(newest.stderr || newest.stdout).trim()}` }],
						isError: true,
					};
				}
				const top = parseReply(newest.stdout).entries[0];
				cursor = top?.seq ?? 0;
				writeCursor(file, cursor);
			}

			// Drain what is already queued. One call returns the whole backlog.
			const drained = await pi.exec("dm", dmArgs([...base, "list", ...filter, "--after", String(cursor), "--json"]), {
				cwd: ctx.cwd,
				signal,
				timeout: 15_000,
			});
			if (drained.code !== 0) {
				return {
					content: [{ type: "text" as const, text: `dm msg list failed: ${(drained.stderr || drained.stdout).trim()}` }],
					isError: true,
				};
			}

			const drainReply = parseReply(drained.stdout);

			// Advance over every drained entry, including states the caller filtered
			// out, so the next call does not see them again. dm's list ignores --state,
			// so a filtered caller must skip them here.
			const drainedEntries = drainReply.entries.sort((a, b) => a.seq - b.seq);
			let next = drainedEntries.length > 0 ? drainedEntries[drainedEntries.length - 1].seq : cursor;
			let entries = params.state ? drainedEntries.filter((e) => e.state === params.state) : drainedEntries;
			let trimmed = drainReply.trimmed;
			let waited = false;

			// Nothing matching queued: block for the next match.
			if (entries.length === 0) {
				waited = true;
				const wait = await pi.exec(
					"dm",
					dmArgs([...base, "wait", ...filter, "--after", String(next), "--timeout", `${seconds}s`, "--json"]),
					{ cwd: ctx.cwd, signal, timeout: (seconds + 20) * 1000 },
				);
				if (wait.code === 0) {
					const waitReply = parseReply(wait.stdout);
					entries = waitReply.entries;
					if (entries.length > 0) next = entries[entries.length - 1].seq;
					trimmed = waitReply.trimmed;
				} else {
					const err = (wait.stderr || wait.stdout).trim();
					if (err.includes("wait timed out")) {
						return {
							content: [
								{
									type: "text" as const,
									text: `No lane events within ${seconds}s. Cursor stays at ${next}. Call again, or run \`dungeon next\` to check the fleet.`,
								},
							],
							details: { cursor: next, waited: true, trimmed: false, entries: [] },
						};
					}
					return {
						content: [{ type: "text" as const, text: `dm msg wait failed: ${err || `exit ${wait.code}`}` }],
						isError: true,
					};
				}
			}

			writeCursor(file, next);

			const lines = entries.map(describe).join("\n");
			const trimNote = trimmed
				? "\nThe cursor fell behind the box's kept rows, so older events were dropped. Read the lane's report instead."
				: "";
			const hint = entries.some((e) => e.state === "done" || e.state === "error")
				? "\nRun `dungeon next` to settle the units whose lanes reported done or error."
				: entries.some((e) => e.state === "blocked")
					? "\nA lane is blocked. Read its pane with `dm read <from>` and answer with `dm write-to <from> --msg \"...\" --enter`, or ask the operator."
					: "";

			return {
				content: [
					{
						type: "text" as const,
						text: `${entries.length} lane event${entries.length === 1 ? "" : "s"} since seq ${cursor}${waited ? " (waited)" : ""}:\n\n${lines}\n\nCursor now ${next}.${trimNote}${hint}`,
					},
				],
				details: { cursor: next, waited, trimmed, entries },
			};
		},
	});
}
