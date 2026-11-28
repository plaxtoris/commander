import { randomUUID } from "node:crypto";
import { existsSync, linkSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export default function (pi) {
	const path = process.env.COMMANDER_PI_SELECTION;
	const provider = "openai-compatible";
	const registry = JSON.parse(readFileSync(join(process.env.PI_CODING_AGENT_DIR, "models.json"), "utf8"));
	const capped = new Set(process.env.COMMANDER_PI_CAPPED_MODELS?.split("\n"));
	const models = new Map(registry.providers[provider].models.map(model => [model.id, model]));
	let outputLimit;

	function showLimits(ctx, model, limit) {
		outputLimit = limit;
		if (!ctx.hasUI) return;
		const context = model?.contextWindow ?? "unknown";
		ctx.ui.setStatus("commander-limits", model ? `context ${context} · output ${outputLimit ?? "server default"}` : undefined);
	}
	function resetLimits(ctx) {
		const model = ctx.model?.provider === provider && models.get(ctx.model.id);
		showLimits(ctx, model, model?.maxTokens);
	}

	function save(ctx, firstOnly = false) {
		if (ctx.model?.provider !== provider || (firstOnly && existsSync(path))) return;
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
		resetLimits(ctx);
		if (event.reason === "startup") save(ctx, !process.env.COMMANDER_PI_EXPLICIT_SELECTION);
	});
	pi.on("model_select", (_, ctx) => {
		resetLimits(ctx);
		save(ctx);
	});
	pi.on("thinking_level_select", (_, ctx) => save(ctx));
	pi.on("before_provider_request", ({ payload }, ctx) => {
		if (ctx.model?.provider !== provider || payload?.model !== ctx.model.id || !Array.isArray(payload.messages)) return;
		const model = models.get(payload.model);
		if (!model) return;
		const requested = payload.max_completion_tokens ?? payload.max_tokens;
		// Pi's context reserve can collapse a usable local window to a single token.
		// Let the server enforce its own ceiling in that case. An explicit cap wins.
		const collapsed = requested === 1 && model.maxTokens > 1 && !capped.has(model.id);
		if (model.maxTokens && !collapsed) {
			showLimits(ctx, model, requested);
			return;
		}
		const { max_tokens, max_completion_tokens, ...request } = payload;
		showLimits(ctx, model);
		return request;
	});
	pi.on("message_end", ({ message }, ctx) => {
		if (message.role !== "assistant" || message.stopReason !== "length" || message.provider !== provider || message.model !== ctx.model?.id) return;
		const used = message.usage?.output;
		const usage = used > 0 ? ` · ${used} output tokens reported` : "";
		const action = ctx.model.reasoning && pi.getThinkingLevel() !== "off"
			? "lower /thinking or ask for a shorter step"
			: "use /compact for more context or ask for a shorter step";
		const note = `response cut short · ${outputLimit ? `requested up to ${outputLimit}` : "server default"}${usage} · ${action}`;
		if (ctx.hasUI) ctx.ui.notify(note, "warning");
		else console.error(note);
	});
}
