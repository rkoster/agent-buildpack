import { tool } from "@opencode-ai/plugin"

const DEFAULT_FACADE_URL = "http://127.0.0.1:18080/v1"
const DEFAULT_EXECD_PORT = 44772
const MAX_FILE_BYTES = 512 * 1024

function requiredEnvironment(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is not configured for the OpenSandbox plugin`)
  return value
}

function facadeURL(path) {
  const base = (process.env.OPEN_SANDBOX_URL || DEFAULT_FACADE_URL).replace(/\/+$/, "")
  return `${base}${path}`
}

function facadeHeaders() {
  const key = process.env.OPEN_SANDBOX_API_KEY
  return key ? { "OPEN-SANDBOX-API-KEY": key } : {}
}

async function checkedFetch(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: { ...facadeHeaders(), ...(options.headers || {}) },
  })
  if (!response.ok) {
    const body = await response.text().catch(() => "")
    throw new Error(`OpenSandbox request failed (${response.status}): ${body || response.statusText}`)
  }
  return response
}

async function endpointFor(sandboxId) {
  const port = Number(process.env.OPEN_SANDBOX_EXECD_PORT || DEFAULT_EXECD_PORT)
  const response = await checkedFetch(
    facadeURL(`/sandboxes/${encodeURIComponent(sandboxId)}/endpoints/${port}`),
  )
  const result = await response.json()
  if (typeof result.endpoint !== "string") throw new Error("OpenSandbox endpoint response did not include endpoint")
  return { ...result, port }
}

function execdUrl(endpoint, path) {
  const base = new URL(endpoint.endpoint)
  base.pathname = `${base.pathname.replace(/\/+$/, "")}/${path.replace(/^\/+/, "")}`
  return base
}

function execdHeaders(endpoint) {
  const headers = { ...(endpoint.headers || {}) }
  const accessToken = process.env.EXECD_ACCESS_TOKEN
  if (accessToken) headers["X-EXECD-ACCESS-TOKEN"] = accessToken
  return headers
}

async function readCommandStream(response) {
  const raw = await response.text()
  const output = []
  const errors = []
  for (const block of raw.split(/\r?\n\r?\n/)) {
    const data = block
      .split(/\r?\n/)
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).replace(/^ /, ""))
      .join("\n")
    if (!data) continue
    let event
    try {
      event = JSON.parse(data)
    } catch {
      output.push(data)
      continue
    }
    if (event.type === "stdout" || event.type === "stderr") output.push(event.text || "")
    if (event.type === "error") errors.push(event.error?.evalue || event.error?.ename || "Command failed")
  }
  if (errors.length) throw new Error(errors.join("\n"))
  return output.join("") || raw
}

export const OpenSandboxPlugin = async () => ({
  tool: {
    sandbox_list: tool({
      description:
        "List existing remote Cloud Foundry OpenSandboxes owned by this agent. Use this to find a sandbox ID before creating another one or running remote commands. Results are paginated.",
      args: {
        page: tool.schema.number().int().positive().optional().describe("Page number, starting at 1"),
        pageSize: tool.schema.number().int().min(1).max(100).optional().describe("Sandboxes per page, up to 100"),
      },
      async execute(args) {
        const url = new URL(facadeURL("/sandboxes"))
        if (args.page !== undefined) url.searchParams.set("page", String(args.page))
        if (args.pageSize !== undefined) url.searchParams.set("pageSize", String(args.pageSize))
        const response = await checkedFetch(url)
        const result = await response.json()
        if (!Array.isArray(result.items) || !result.pagination) {
          throw new Error("OpenSandbox list response is missing items or pagination")
        }
        return {
          title: "Remote sandboxes",
          output: JSON.stringify({ items: result.items, pagination: result.pagination }),
          metadata: { pagination: result.pagination },
        }
      },
    }),

    sandbox_create: tool({
      description:
        "Create a remote Cloud Foundry OpenSandbox using the configured sandbox image. Use this before running sandbox commands or transferring files. The sandbox is separate from OpenCode's local project directory.",
      args: {
        name: tool.schema.string().optional().describe("Optional short purpose label to return to the user"),
      },
      async execute(args) {
        const image = requiredEnvironment("CF_SANDBOX_IMAGE")
        const entrypoint = (process.env.CF_SANDBOX_ENTRYPOINT || "/opt/opensandbox/bootstrap")
          .trim()
          .split(/\s+/)
        const response = await checkedFetch(facadeURL("/sandboxes"), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ image: { uri: image }, entrypoint }),
        })
        const sandbox = await response.json()
        if (!sandbox.id) throw new Error("OpenSandbox create response did not include a sandbox ID")
        return {
          title: "Remote sandbox created",
          output: `Sandbox ID: ${sandbox.id}${args.name ? `\nPurpose: ${args.name}` : ""}\nThe workspace is remote and is not automatically synchronized with the local OpenCode project.`,
          metadata: { sandboxId: sandbox.id },
        }
      },
    }),

    sandbox_command: tool({
      description:
        "Run a shell command in a remote OpenSandbox. This does not run in the OpenCode app container. Create a sandbox first and pass its ID.",
      args: {
        sandboxId: tool.schema.string().describe("ID returned by sandbox_create"),
        command: tool.schema.string().describe("Shell command to execute in the remote sandbox"),
        cwd: tool.schema.string().optional().describe("Remote working directory, for example /workspace"),
        timeoutMs: tool.schema.number().int().positive().optional().describe("Maximum command duration in milliseconds"),
      },
      async execute(args) {
        const endpoint = await endpointFor(args.sandboxId)
        const response = await checkedFetch(execdUrl(endpoint, "command"), {
          method: "POST",
          headers: { ...execdHeaders(endpoint), "Content-Type": "application/json", Accept: "text/event-stream" },
          body: JSON.stringify({
            command: args.command,
            cwd: args.cwd,
            timeout: args.timeoutMs,
            background: false,
          }),
        })
        return {
          title: "Remote command completed",
          output: await readCommandStream(response),
          metadata: { sandboxId: args.sandboxId, cwd: args.cwd || "image default" },
        }
      },
    }),

    sandbox_read_file: tool({
      description: "Read a file from the remote OpenSandbox, not the OpenCode app filesystem.",
      args: {
        sandboxId: tool.schema.string().describe("ID returned by sandbox_create"),
        path: tool.schema.string().describe("Path to a file in the remote sandbox"),
      },
      async execute(args) {
        const endpoint = await endpointFor(args.sandboxId)
        const url = execdUrl(endpoint, "files/download")
        url.searchParams.set("path", args.path)
        const response = await checkedFetch(url, { headers: execdHeaders(endpoint) })
        const bytes = new Uint8Array(await response.arrayBuffer())
        if (bytes.byteLength > MAX_FILE_BYTES) {
          throw new Error(`File is ${bytes.byteLength} bytes; the plugin read limit is ${MAX_FILE_BYTES} bytes`)
        }
        return {
          title: `Remote file: ${args.path}`,
          output: new TextDecoder().decode(bytes),
          metadata: { sandboxId: args.sandboxId, path: args.path, bytes: bytes.byteLength },
        }
      },
    }),

    sandbox_write_file: tool({
      description: "Write or replace a text file in the remote OpenSandbox, not the local OpenCode project.",
      args: {
        sandboxId: tool.schema.string().describe("ID returned by sandbox_create"),
        path: tool.schema.string().describe("Absolute or sandbox-relative destination path"),
        content: tool.schema.string().describe("UTF-8 text to write"),
      },
      async execute(args) {
        const contentBytes = Buffer.byteLength(args.content)
        if (contentBytes > MAX_FILE_BYTES) {
          throw new Error(`Content is ${contentBytes} bytes; the plugin write limit is ${MAX_FILE_BYTES} bytes`)
        }
        const endpoint = await endpointFor(args.sandboxId)
        const form = new FormData()
        form.append(
          "metadata",
          new Blob([JSON.stringify({ path: args.path })], { type: "application/json" }),
          "metadata.json",
        )
        form.append("file", new Blob([args.content], { type: "application/octet-stream" }), "content")
        const response = await checkedFetch(execdUrl(endpoint, "files/upload"), {
          method: "POST",
          headers: execdHeaders(endpoint),
          body: form,
        })
        return {
          title: `Remote file written: ${args.path}`,
          output: await response.text().catch(() => "File uploaded successfully"),
          metadata: { sandboxId: args.sandboxId, path: args.path, bytes: contentBytes },
        }
      },
    }),

    sandbox_delete: tool({
      description: "Delete a remote OpenSandbox after the work is complete. Its workspace is ephemeral.",
      args: { sandboxId: tool.schema.string().describe("ID returned by sandbox_create") },
      async execute(args) {
        await checkedFetch(facadeURL(`/sandboxes/${encodeURIComponent(args.sandboxId)}`), { method: "DELETE" })
        return { title: "Remote sandbox deleted", output: `Deleted sandbox ${args.sandboxId}.` }
      },
    }),
  },

  "experimental.chat.system.transform": async (_input, output) => {
    output.system.push(
      [
        "## Cloud Foundry OpenSandbox tools",
        "This app provides remote sandbox tools. First use sandbox_list to check for an existing sandbox; if needed create one with sandbox_create. Use only sandbox_command, sandbox_read_file, and sandbox_write_file with its ID for shell or file operations requested by the user.",
        "The remote sandbox is not automatically synchronized with the local OpenCode project. Do not use local shell/read/write/edit tools as a substitute for remote sandbox operations. Explain that files must be transferred explicitly.",
        "When finished, use sandbox_delete if the user does not need the ephemeral sandbox anymore.",
      ].join("\n"),
    )
  },
})
