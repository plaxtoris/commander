import { randomUUID } from "node:crypto";
import { existsSync, linkSync, renameSync, rmSync, writeFileSync } from "node:fs";

export default function (pi) {
	const path = process.env.COMMANDER_PI_SELECTION;
	function save(ctx, firstOnly = false) {
		if (ctx.model?.provider !== "openai-compatible" || (firstOnly && existsSync(path))) return;
		const selection = { model: ctx.model.id, thinking: pi.getThinkingLevel() };
		const temp = path + "." + randomUUID();
		writeFileSync(temp, JSON.stringify(selection), { flag: "wx", mode: 0o600 });
		try {
			if (firstOnly) linkSync(temp, path);
			else renameSync(temp, path);
		} catch (err) {
			if (!firstOnly || err.code !== "EEXIST") throw err;
		} finally {
			rmSync(temp, { force: true });
		}
	}
	pi.on("session_start", (event, ctx) => {
		if (event.reason === "startup") save(ctx, !process.env.COMMANDER_PI_EXPLICIT_SELECTION);
	});
	pi.on("model_select", (_, ctx) => save(ctx));
	pi.on("thinking_level_select", (_, ctx) => save(ctx));
}
