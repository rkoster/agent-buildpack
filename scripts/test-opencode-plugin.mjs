import assert from "node:assert/strict"
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import { pathToFileURL } from "node:url"

const temp = await mkdtemp(path.join(os.tmpdir(), "opencode-plugin-test-"))
try {
  const pluginDir = path.join(temp, "opencode", "plugins")
  const packageDir = path.join(temp, "node_modules", "@opencode-ai", "plugin")
  await mkdir(pluginDir, { recursive: true })
  await mkdir(packageDir, { recursive: true })
  await writeFile(path.join(temp, "package.json"), '{"type":"module"}')
  await writeFile(path.join(packageDir, "package.json"), '{"type":"module","exports":"./index.js"}')
  await writeFile(
    path.join(packageDir, "index.js"),
    [
      'const schema = { string: () => chain(), number: () => chain() }',
      'function chain() { return { optional() { return this }, describe() { return this }, int() { return this }, positive() { return this }, min() { return this }, max() { return this } } }',
      'export const tool = (definition) => definition',
      'tool.schema = schema',
    ].join("\n"),
  )
  const pluginFile = new URL("../opencode/plugins/opensandbox.js", import.meta.url)
  await writeFile(path.join(pluginDir, "opensandbox.js"), await readFile(pluginFile))

  process.env.CF_SANDBOX_IMAGE = "example.test/sandbox@sha256:abc"
  process.env.OPEN_SANDBOX_API_KEY = "facade-test-key"
  process.env.EXECD_ACCESS_TOKEN = "execd-test-token"
  const requests = []
  globalThis.fetch = async (input, init = {}) => {
    const url = new URL(input)
    requests.push({ url, init })
    if (url.pathname === "/v1/sandboxes" && init.method === "POST") {
      return Response.json({ id: "sandbox-123" }, { status: 202 })
    }
    if (url.pathname === "/v1/sandboxes" && !init.method) {
      return Response.json({
        items: [{ id: "sandbox-123", status: { state: "Running" } }],
        pagination: { page: Number(url.searchParams.get("page") || 1), pageSize: Number(url.searchParams.get("pageSize") || 20), totalItems: 1, hasNextPage: false },
      })
    }
    if (url.pathname === "/v1/sandboxes/sandbox-123/endpoints/44772") {
      return Response.json({
        endpoint: "http://127.0.0.1:18080/v1/sandboxes/sandbox-123/proxy/44772",
        headers: { "OPEN-SANDBOX-API-KEY": "facade-test-key" },
      })
    }
    if (url.pathname.endsWith("/command")) {
      return new Response('data: {"type":"stdout","text":"remote output\\n"}\n\ndata: {"type":"execution_complete"}\n\n', {
        headers: { "Content-Type": "text/event-stream" },
      })
    }
    if (url.pathname.endsWith("/files/download")) return new Response("remote file contents")
    if (url.pathname.endsWith("/files/upload")) return new Response("uploaded", { status: 200 })
    if (url.pathname === "/v1/sandboxes/sandbox-123" && init.method === "DELETE") {
      return new Response(null, { status: 204 })
    }
    throw new Error(`unexpected request ${init.method || "GET"} ${url}`)
  }

  const { OpenSandboxPlugin } = await import(pathToFileURL(path.join(pluginDir, "opensandbox.js")))
  const hooks = await OpenSandboxPlugin()
  assert.deepEqual(
    Object.keys(hooks.tool).sort(),
    ["sandbox_command", "sandbox_create", "sandbox_delete", "sandbox_list", "sandbox_read_file", "sandbox_write_file"],
  )

  const listed = await hooks.tool.sandbox_list.execute({ page: 2, pageSize: 10 })
  const listing = JSON.parse(listed.output)
  assert.equal(listing.items[0].id, "sandbox-123")
  assert.deepEqual(listing.pagination, { page: 2, pageSize: 10, totalItems: 1, hasNextPage: false })
  assert.equal(requests[0].url.search, "?page=2&pageSize=10")
  assert.equal(requests[0].init.headers["OPEN-SANDBOX-API-KEY"], "facade-test-key")

  const created = await hooks.tool.sandbox_create.execute({ name: "demo" })
  assert.match(created.output, /sandbox-123/)
  assert.equal(JSON.parse(requests[1].init.body).image.uri, process.env.CF_SANDBOX_IMAGE)
  assert.equal(requests[1].init.headers["OPEN-SANDBOX-API-KEY"], "facade-test-key")

  const command = await hooks.tool.sandbox_command.execute({
    sandboxId: "sandbox-123",
    command: "ruby -v",
    cwd: "/workspace",
  })
  assert.match(command.output, /remote output/)
  const commandRequest = requests.find((request) => request.url.pathname.endsWith("/command"))
  assert.equal(commandRequest.init.headers["X-EXECD-ACCESS-TOKEN"], "execd-test-token")
  assert.equal(JSON.parse(commandRequest.init.body).cwd, "/workspace")

  const read = await hooks.tool.sandbox_read_file.execute({ sandboxId: "sandbox-123", path: "/workspace/app.rb" })
  assert.equal(read.output, "remote file contents")

  await hooks.tool.sandbox_write_file.execute({
    sandboxId: "sandbox-123",
    path: "/workspace/app.rb",
    content: "puts 'remote'",
  })
  const upload = requests.find((request) => request.url.pathname.endsWith("/files/upload"))
  assert.ok(upload.init.body instanceof FormData)

  await hooks.tool.sandbox_delete.execute({ sandboxId: "sandbox-123" })
  assert.equal(requests.at(-1).init.method, "DELETE")
  console.log("OpenSandbox OpenCode plugin checks passed")
} finally {
  await rm(temp, { recursive: true, force: true })
}
