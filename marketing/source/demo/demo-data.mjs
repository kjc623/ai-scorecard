// Sample data for the demo console: Northwind, a fictional company of about 1,200 people on
// Windows laptops. Every person, team, device, prompt and figure here is invented.
//
// One seeded simulation of every prompt sent over the last 100 days is built for the current UTC
// day, and every answer is counted from it, so the screens agree with each other: the tools add up
// to the prompts, the buckets to the window, a team's people to its members. Times are relative to
// the request: data runs up to a few minutes ago, and the story (the ChatGPT redirect rule, the
// audit trail) sits a fixed number of working days back.

import { createHash } from 'node:crypto';

import { coverage, freshness } from '../../../services/dashboard/test/fixtures.mjs';
import { EXPORT_MAX_ROWS, EXPORT_TTL_MS, listExportCsv } from '../../../services/query-api/src/export.js';

export const SESSION = Object.freeze({ actor: 'Dana Reyes', roles: ['admin'] });

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
/** How far back prompts are simulated: the widest window the console offers, and a little more. */
const HISTORY_DAYS = 100;
/** How long a prompt takes to reach the aggregates. */
const LAG = 3 * MINUTE;
const DOMAIN = 'northwind.example';
const TENANT_ID = '6d2f9a14-3c8b-4e71-9f05-b21e7c4a8d36';
const ENTRA_TENANT_ID = 'c4e8b1d2-57a3-4f9e-8b06-3d1f2a7c9e54';
const AGENT_VERSION = '1.0.78';
const DANA = `dana.reyes@${DOMAIN}`;
const ARJUN = `arjun.mehta@${DOMAIN}`;
/** The ChatGPT redirect rule took effect this many working days back, at this hour (UTC). */
const REDIRECT_WORKDAYS_AGO = 3;
const REDIRECT_HOUR = 10;

// ── Seeded randomness ─────────────────────────────────────────────────────────────────────────

/** mulberry32: small, seedable, and good enough to spread sample rows. */
function random(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6D2B79F5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** FNV-1a over the parts, so a seed names what it is for. */
function seedOf(...parts) {
  let h = 2166136261;
  for (const c of parts.join('|')) h = Math.imul(h ^ c.charCodeAt(0), 16777619);
  return h >>> 0;
}

const pick = (rand, list) => list[Math.floor(rand() * list.length)];
const hex = (rand, n) => Array.from({ length: n }, () => Math.floor(rand() * 16).toString(16)).join('');
const uuid = (rand) => `${hex(rand, 8)}-${hex(rand, 4)}-4${hex(rand, 3)}-${'89ab'[Math.floor(rand() * 4)]}${hex(rand, 3)}-${hex(rand, 12)}`;

/** An index drawn in proportion to the weights. */
function weighted(rand, weights) {
  let r = rand() * weights.reduce((a, b) => a + b, 0);
  for (let i = 0; i < weights.length; i += 1) {
    r -= weights[i];
    if (r < 0) return i;
  }
  return weights.length - 1;
}

function poisson(rand, mean) {
  const limit = Math.exp(-mean);
  let k = 0;
  for (let p = rand(); p > limit; p *= rand()) k += 1;
  return k;
}

const iso = (ms) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
const dayOf = (ms) => Math.floor(ms / DAY) * DAY;
const isWeekend = (ms) => [0, 6].includes(new Date(ms).getUTCDay());

/** The start of the day n working days before today, so the story's events fall on weekdays. */
function workdaysBack(today, n) {
  let day = today;
  for (let left = n; left > 0;) {
    day -= DAY;
    if (!isWeekend(day)) left -= 1;
  }
  return day;
}

// ── The tools, the rules and the labels ──────────────────────────────────────────────────────

// The tools in use, by catalogue key and name, with the decision on each (null: none yet), the
// routes a device collects them by, and the size of a prompt in bytes.
const TOOLS = Object.freeze([
  { key: 'microsoft_copilot', name: 'Microsoft Copilot', sanction: 'sanctioned', routes: ['ext.page_context'], bytes: [700, 2600] },
  { key: 'm365_copilot', name: 'Microsoft 365 Copilot', sanction: 'sanctioned', routes: ['ext.page_context'], bytes: [900, 4200] },
  { key: 'chatgpt_web', name: 'ChatGPT (web)', sanction: 'unsanctioned', routes: ['ext.web_request'], bytes: [700, 3000] },
  { key: 'claude_web', name: 'Claude (web)', sanction: null, routes: ['ext.web_request'], bytes: [900, 5200] },
  { key: 'gemini_web', name: 'Gemini (web)', sanction: 'unsanctioned', routes: ['ext.web_request'], bytes: [600, 2400] },
  { key: 'perplexity_web', name: 'Perplexity (web)', sanction: null, routes: ['ext.web_request'], bytes: [300, 1200] },
  { key: 'claude_code', name: 'Claude Code', sanction: 'sanctioned', routes: ['tool.hook', 'tool.otel'], bytes: [6000, 42000] },
  { key: 'codex', name: 'Codex CLI', sanction: null, routes: ['tool.hook'], bytes: [5000, 30000] },
  { key: 'cursor', name: 'Cursor', sanction: null, routes: ['tool.hook'], bytes: [4000, 26000] },
].map(Object.freeze));
const TOOL_INDEX = new Map(TOOLS.map((tool, i) => [tool.key, i]));

/** A tool parameter as a page sends it, `app:<key>` or the key itself, as its index; -1 for none of these. */
function toolIndexOf(value) {
  return TOOL_INDEX.get(String(value).replace(/^app:/, '')) ?? -1;
}

// Every tool in the catalogue, as Policy lists them for a decision, with the fingerprints the
// catalogue knows for the tools Northwind uses.
const CATALOGUE = Object.freeze([
  ['aws_bedrock', 'Amazon Bedrock'], ['anthropic_api', 'Anthropic API'], ['azure_ai_foundry', 'Azure AI Foundry'],
  ['chatgpt_web', 'ChatGPT (web)'], ['chatgpt_desktop', 'ChatGPT Desktop'], ['claude_web', 'Claude (web)'], ['claude_code', 'Claude Code'],
  ['claude_code_vscode', 'Claude Code for VS Code'], ['claude_desktop', 'Claude Desktop'], ['codex', 'Codex CLI'], ['cohere_api', 'Cohere API'],
  ['continue', 'Continue'], ['cursor', 'Cursor'], ['deepseek_api', 'DeepSeek API'], ['gemini_web', 'Gemini (web)'], ['gemini_cli', 'Gemini CLI'],
  ['gemini_code_assist', 'Gemini Code Assist'], ['github_copilot', 'GitHub Copilot'], ['copilot_cli', 'GitHub Copilot CLI'], ['google_ai_api', 'Google AI API'],
  ['groq_api', 'Groq API'], ['jetbrains', 'JetBrains IDEs'], ['llama_cpp', 'llama.cpp'], ['lm_studio', 'LM Studio'], ['m365_copilot', 'Microsoft 365 Copilot'],
  ['microsoft_copilot', 'Microsoft Copilot'], ['mistral_api', 'Mistral API'], ['ollama', 'Ollama'], ['openai_api', 'OpenAI API'], ['openrouter', 'OpenRouter'],
  ['perplexity_web', 'Perplexity (web)'], ['perplexity_api', 'Perplexity API'], ['together_ai', 'Together AI'], ['vscode', 'Visual Studio Code'],
  ['vllm', 'vLLM'], ['windsurf', 'Windsurf'], ['xai_api', 'xAI API'],
]);
const FINGERPRINTS = Object.freeze({
  chatgpt_web: ['tls_11574658dafb8805', 'tls_2af2dd0ea445e033', 'tls_fd863543bed5e1fd', 'tf2:a64fvqypthxcqzcwgpxxeeo4kvhk5onujlq22qafww3ql422pxyq', 'tf2:de4s6yclxfhnumvdm3ubzgd5jveiwkzapelxdcj2qdzd72qrridq', 'tf2:mespeqbrgr4vb7rqmmrubxuglidtpu3e5xpjd7dwqkm2evicbwla'],
  claude_code: ['tls_31299ef7601928f7', 'tls_5a5a41ed0bf50d9d', 'tls_69f6ab029df3c019', 'tls_b6681b043244c43f', 'tls_cb53d2b2add3d450'],
  gemini_web: ['tf2:6lwajg7v6mwycbn43eszknx6p3sqfegfsk6tj2p4yqovdd32mnfq', 'tls_7659f7a5e64cd885'],
  perplexity_web: ['tf2:vjlb4hsqpngtmkhd4gwtgmcorqo26q2ezy7vp4erdqysbawzdx2q', 'tls_a40a04ef6e27de9f'],
  cursor: ['tls_14a9086e088f0d82', 'tls_9230903190dcf0dc'],
});

const DATA_CLASSES = Object.freeze(['credential', 'customer_pii', 'government_id', 'health', 'legal_commercial', 'payment_card', 'source_code']);

const FALCON = Object.freeze({
  label: 'custom_project_falcon',
  display_name: 'Project Falcon',
  words: ['project falcon', 'falcon deal'],
  customer_names: ['Halden Freight'],
  file_extensions: [],
  file_names: ['falcon-board-pack.pptx'],
});

// The enforcement rules in the order a device applies them.
const RULES = Object.freeze([
  { rule_id: 'block_credentials', action: 'block', match: { labels: ['credential'] }, message: 'This prompt contains a password, key or token. Remove it before you send it: secrets must never go into an AI tool.' },
  { rule_id: 'redirect_chatgpt_to_copilot', action: 'redirect', match: { tools: ['chatgpt_web'] }, message: "ChatGPT isn't approved for company work. Use Microsoft Copilot instead.", link: 'https://copilot.microsoft.com/' },
  { rule_id: 'warn_customer_data', action: 'warn', match: { labels: ['customer_pii'] }, message: 'This prompt looks like it holds customer personal data. Only send it if you need that data for the task.' },
  { rule_id: 'log_code_undecided', action: 'allow', match: { labels: ['source_code'], tools: ['codex', 'cursor'] }, message: '' },
].map(Object.freeze));
const OUTCOME = Object.freeze({ block: 'blocked', redirect: 'blocked', warn: 'warned', allow: 'logged' });

/** The classifier rule a finding names, by the class it found. A custom label raises no finding. */
const DETECTORS = Object.freeze({
  credential: [['AWS_ACCESS_KEY_ID', 'AWS access key ID'], ['PRIVATE_KEY_BLOCK', 'Private key']],
  customer_pii: [['CUSTOMER_PII_LABEL', 'Personal data field']],
  payment_card: [['PAYMENT_CARD_PAN', 'Payment card number']],
  source_code: [['SOURCE_DECLARATION', 'Source code']],
});
const SEVERITY = Object.freeze({ credential: 'critical', customer_pii: 'high', payment_card: 'critical', source_code: 'high' });
const SCORE = Object.freeze({ credential: 0.95, customer_pii: 0.65, payment_card: 0.9, source_code: 0.7, [FALCON.label]: 1 });

// ── The organisation ────────────────────────────────────────────────────────────────────────

const teamId = (n) => `7a3c5e10-4b2d-4e8f-9a61-5d0c8b2e${String(n).padStart(4, '0')}`;

// The teams, each matched on a directory department (Engineering on an Entra group), with how many
// people are in it, the share of them who use AI at all, and the tools they reach for beyond the
// company's usual ones: [chance a person uses it, weight of their prompts].
const TEAMS = Object.freeze([
  { id: teamId(1), name: 'Engineering', size: 310, adoption: 0.8, tools: { claude_code: [0.45, 6], cursor: [0.2, 4], codex: [0.1, 4], claude_web: [0.25, 2], m365_copilot: [0.15, 2] } },
  { id: teamId(2), name: 'Sales', size: 210, adoption: 0.59, tools: { perplexity_web: [0.15, 2] } },
  { id: teamId(3), name: 'Customer Success', size: 170, adoption: 0.65, tools: {} },
  { id: teamId(4), name: 'Operations', size: 140, adoption: 0.48, tools: {} },
  { id: teamId(5), name: 'Finance', size: 95, adoption: 0.52, tools: {} },
  { id: teamId(6), name: 'Marketing', size: 90, adoption: 0.72, tools: { claude_web: [0.3, 3], perplexity_web: [0.18, 2], gemini_web: [0.12, 2] } },
  { id: teamId(7), name: 'People', size: 55, adoption: 0.48, tools: {} },
  { id: teamId(8), name: 'Legal', size: 50, adoption: 0.43, tools: {} },
].map(Object.freeze));
/** Departments no team is made from: their people count in no team. */
const OTHER_DEPARTMENTS = Object.freeze([['Executive Office', 14, 0.5], ['Facilities', 20, 0.25], ['Procurement', 26, 0.45]]);
const BASE_TOOLS = Object.freeze({ microsoft_copilot: [0.5, 3], m365_copilot: [0.38, 3], chatgpt_web: [0.32, 3], claude_web: [0.1, 2], gemini_web: [0.06, 2], perplexity_web: [0.08, 1.5] });
const ENGINEERING_GROUP = Object.freeze({ id: 'grp-all-engineering', object_id: '0f6d8a2c-3e91-4b57-a1c4-7d2e9b5f3a10', name: 'All Engineering' });

/** The chance a prompt carries each class, by the sender's department: credential, customer data, card number, Project Falcon. */
const RISK = Object.freeze({
  Engineering: [0.0057, 0.0007, 0, 0],
  Sales: [0.0009, 0.0104, 0, 0.0005],
  'Customer Success': [0.0009, 0.0226, 0.0015, 0],
  Operations: [0.0032, 0.0038, 0.0005, 0.0005],
  Finance: [0.0013, 0.0074, 0.004, 0.006],
  Marketing: [0.0005, 0.0054, 0, 0.001],
  People: [0.0005, 0.011, 0, 0.001],
  Legal: [0.0005, 0.0054, 0, 0.008],
  'Executive Office': [0.0009, 0.0018, 0, 0.02],
  Facilities: [0.0009, 0.0018, 0, 0],
  Procurement: [0.0009, 0.0029, 0.002, 0.002],
});
/** The chance a prompt to each tool holds pasted source code, for an engineer; a tenth of it for anyone else. */
const SOURCE = Object.freeze({ claude_code: 0.01, cursor: 0.047, codex: 0.047, claude_web: 0.017, chatgpt_web: 0.01, microsoft_copilot: 0.004, perplexity_web: 0.004 });

// The people the story names, in the departments they work in. The rest of the directory is drawn
// from the name lists below.
const CAST = Object.freeze([
  ['Dana Reyes', 'Operations'], ['Arjun Mehta', 'Operations'], ['Mia Novak', 'Operations'],
  ['Wei Zhang', 'Engineering'], ['Aisha Bello', 'Engineering'], ['Lukas Brandt', 'Engineering'], ['Sofia Marquez', 'Engineering'],
  ['Kenji Watanabe', 'Engineering'], ['Noah Fischer', 'Engineering'], ['Yusuf Demir', 'Engineering'],
  ['Olivia Brennan', 'Sales'], ['Rahul Iyer', 'Sales'], ['Hannah Kowalski', 'Sales'],
  ['Grace Adeyemi', 'Customer Success'], ['Mateo Silva', 'Customer Success'], ['Chloe Dubois', 'Customer Success'], ['Ingrid Solberg', 'Customer Success'],
  ['Daniel Osei', 'Finance'], ['Emma Lindqvist', 'Finance'], ['Fatima Rahman', 'Legal'], ['Jonas Weber', 'Legal'],
  ['Leila Nasser', 'Marketing'], ['Ben Carter', 'Marketing'], ['Ana Costa', 'People'], ['Samuel Nwosu', 'People'],
]);
const FIRST = Object.freeze([
  'Aaliyah', 'Abdul', 'Abena', 'Adaeze', 'Adam', 'Adrian', 'Agnes', 'Ahmed', 'Aiko', 'Aisling', 'Akira', 'Alba',
  'Alejandro', 'Aleksander', 'Alice', 'Alina', 'Amani', 'Amara', 'Amir', 'Andile', 'Andrea', 'Anika', 'Anja', 'Anton',
  'Arash', 'Ari', 'Arnav', 'Asha', 'Astrid', 'Aurelie', 'Ayumi', 'Bao', 'Beatriz', 'Benedikt', 'Bianca', 'Bilal',
  'Bjorn', 'Bongani', 'Callum', 'Camila', 'Carlos', 'Carmen', 'Cecilia', 'Chen', 'Chiara', 'Chidi', 'Ciaran', 'Clara',
  'Cristina', 'Dalia', 'Damir', 'Dara', 'David', 'Deepa', 'Diego', 'Dmitri', 'Ebele', 'Edgar', 'Eilidh', 'Eitan',
  'Elena', 'Elias', 'Elif', 'Eliza', 'Emeka', 'Emil', 'Erik', 'Esther', 'Fabian', 'Farah', 'Fatou', 'Federico',
  'Felix', 'Fernanda', 'Fiona', 'Florin', 'Freya', 'Gabriel', 'Gita', 'Goran', 'Greta', 'Hamza', 'Hana', 'Hannes',
  'Haruto', 'Helga', 'Hugo', 'Ibrahim', 'Idris', 'Ilse', 'Imani', 'Ines', 'Irina', 'Isabel', 'Isak', 'Ivan', 'Jakub',
  'Jana', 'Jasmin', 'Javier', 'Jesper', 'Jiho', 'Jin', 'Joaquin', 'Joel', 'Johanna', 'Josefin', 'Julia', 'Kai',
  'Kamal', 'Kareem', 'Karin', 'Kasia', 'Katarina', 'Kemal', 'Kiran', 'Kofi', 'Kwame', 'Laila', 'Lara', 'Laszlo',
  'Leah', 'Lena', 'Leon', 'Lina', 'Linnea', 'Lior', 'Luca', 'Lucia', 'Lwazi', 'Maja', 'Malia', 'Malik', 'Marek',
  'Maria', 'Marisol', 'Maya', 'Mei', 'Milan', 'Mina', 'Minh', 'Miriam', 'Mohammed', 'Nadia', 'Natalia', 'Nikhil',
  'Nils', 'Nkechi', 'Noor', 'Nora', 'Omar', 'Oskar', 'Olu', 'Paula', 'Pedro', 'Petra', 'Priya', 'Quentin', 'Rafael',
  'Rania', 'Ravi', 'Reza', 'Rhea', 'Rohan', 'Ronja', 'Rosa', 'Ruth', 'Saanvi', 'Salma', 'Samira', 'Sana', 'Santiago',
  'Sara', 'Selin', 'Shira', 'Siddharth', 'Silvia', 'Simon', 'Soren', 'Stella', 'Suki', 'Sven', 'Tamar', 'Tariq',
  'Teodor', 'Thea', 'Tobias', 'Tomas', 'Tuan', 'Uma', 'Valentina', 'Vera', 'Victor', 'Wanjiru', 'Xavier', 'Yara',
  'Yasmin', 'Yohannes', 'Yuki', 'Zainab', 'Zeynep', 'Zoe', 'Zofia',
]);
const LAST = Object.freeze([
  'Abara', 'Abiodun', 'Achterberg', 'Agarwal', 'Akhtar', 'Alonso', 'Alvarado', 'Amadi', 'Andersen', 'Aoki', 'Arslan',
  'Asante', 'Aydin', 'Bajwa', 'Bakshi', 'Balogun', 'Banerjee', 'Barros', 'Bauer', 'Becker', 'Bergman', 'Bergstrom',
  'Bianchi', 'Blom', 'Bondarenko', 'Brennan', 'Carvalho', 'Castellano', 'Cerny', 'Chatterjee', 'Chaudhry', 'Dahl',
  'Danjuma', 'Dimitrov', 'Dogan', 'Duarte', 'Eklund', 'Ekstrom', 'Engel', 'Esposito', 'Eze', 'Falk', 'Farouk',
  'Ferreira', 'Fontaine', 'Fujita', 'Gallagher', 'Gashi', 'Gomes', 'Grewal', 'Guerrero', 'Gupta', 'Hakimi',
  'Halvorsen', 'Hansen', 'Hartmann', 'Hashemi', 'Haugen', 'Holm', 'Horvat', 'Hossain', 'Huynh', 'Ibarra', 'Iqbal',
  'Ishikawa', 'Ito', 'Jansen', 'Joshi', 'Jovanovic', 'Kamau', 'Karimi', 'Kaur', 'Keller', 'Kimura', 'Kiplagat', 'Koh',
  'Kovac', 'Kowalczyk', 'Krause', 'Kristiansen', 'Kumar', 'Lachance', 'Laine', 'Lehmann', 'Lim', 'Lindholm', 'Lopes',
  'Lund', 'Mahlangu', 'Marin', 'Martens', 'Medina', 'Mensah', 'Mirza', 'Moller', 'Moreau', 'Mukherjee', 'Mwangi',
  'Nair', 'Nakamura', 'Navarro', 'Ndlovu', 'Nguyen', 'Nieminen', 'Nilsen', 'Novotny', 'Nowak', 'Nunez', 'Obi',
  'Ogunleye', 'Okafor', 'Oliveira', 'Olsen', 'Onyango', 'Ortega', 'Ozturk', 'Pak', 'Patel', 'Pellegrini', 'Peralta',
  'Pereira', 'Pham', 'Popescu', 'Qureshi', 'Quinlan', 'Rahimi', 'Rasmussen', 'Richter', 'Rocha', 'Ruiz', 'Sahin',
  'Saito', 'Salazar', 'Sato', 'Schmid', 'Schultz', 'Sen', 'Sharma', 'Sokolova', 'Stoica', 'Suzuki', 'Svensson',
  'Takahashi', 'Tan', 'Tanaka', 'Teixeira', 'Tran', 'Turan', 'Uddin', 'Ueda', 'Varga', 'Vasquez', 'Vidal', 'Virtanen',
  'Vogel', 'Volkov', 'Vu', 'Wagner', 'Wang', 'Weiss', 'Wojcik', 'Yamamoto', 'Yildiz', 'Yilmaz', 'Zhou', 'Zielinski',
  'Zubair',
]);

// When prompts are sent, by hour of the day (UTC): an office day, and a quiet weekend.
const WEEKDAY_HOURS = Object.freeze([0, 0, 0, 0, 0, 0, 1, 4, 10, 15, 16, 15, 10, 13, 15, 15, 13, 8, 4, 2, 2, 1, 1, 0]);
const WEEKEND_HOURS = Object.freeze([0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 4, 3, 3, 3, 3, 2, 2, 1, 1, 1, 0, 0, 0]);

// The collectors every enrolled laptop runs, and the tool settings an engineer's laptop adds.
const COLLECTORS = Object.freeze(['capture_extension', 'classifier_host', 'cli_shim', 'desktop_proxy', 'egress_proxy', 'flow_monitor', 'hook_relay',
  'inventory_scanner', 'loopback_broker', 'otel_receiver', 'process_detector', 'user_helper']);
const DEVELOPER_COLLECTORS = Object.freeze(['tool_config_claude_code', 'tool_config_codex', 'tool_config_copilot', 'tool_config_cursor']);

// ── The world: people, devices and every prompt ──────────────────────────────────────────────

function buildPeople() {
  const rand = random(seedOf('people'));
  const taken = new Set(CAST.map(([name]) => name));
  const people = [];
  const departments = [...TEAMS.map((team) => [team.name, team.size, team.adoption, team]), ...OTHER_DEPARTMENTS.map(([name, size, adoption]) => [name, size, adoption, null])];
  for (const [department, size, adoption, team] of departments) {
    const cast = CAST.filter(([, d]) => d === department).map(([name]) => name);
    for (let i = 0; i < size; i += 1) {
      let name = cast[i];
      while (!name) {
        const candidate = `${pick(rand, FIRST)} ${pick(rand, LAST)}`;
        if (!taken.has(candidate)) name = candidate;
      }
      taken.add(name);
      const level = i < cast.length ? 0.2 + rand() * 0.5 : rand();
      const tools = [];
      for (const [key, [chance, weight]] of Object.entries({ ...BASE_TOOLS, ...(team?.tools ?? {}) })) {
        if (rand() < chance) tools.push([key, weight]);
      }
      if (tools.length === 0) tools.push(['microsoft_copilot', 1]);
      people.push({
        index: people.length,
        subject: `u_${hex(rand, 32)}`,
        name,
        email: `${name.toLowerCase().replace(' ', '.')}@${DOMAIN}`,
        department,
        team: team?.id ?? null,
        cast: i < cast.length,
        ai: i < cast.length || rand() < adoption,
        // Heavy, regular and light users: the chance of a working day with AI, and prompts on it.
        activity: level < 0.38 ? 0.85 : level < 0.73 ? 0.55 : 0.2,
        rate: level < 0.38 ? 9.8 : level < 0.73 ? 5.45 : 2.7,
        // Most people used AI before the history starts; the rest took it up since.
        startDaysAgo: rand() < 0.82 ? Infinity : Math.floor(rand() * HISTORY_DAYS),
        tools: tools.map(([key]) => key),
        weights: tools.map(([, weight]) => weight),
        devices: [],
        awayDaysAgo: null,
      });
    }
  }
  // Two dozen people are away: their laptops have been quiet for days.
  for (const person of people.filter((p) => !p.cast).sort(() => rand() - 0.5).slice(0, 24)) person.awayDaysAgo = 2 + Math.floor(rand() * 11);
  return people.sort((a, b) => a.name.localeCompare(b.name)).map((person, i) => Object.assign(person, { index: i }));
}

/** A laptop for everyone, a desktop for some engineers, and a few laptops enrolled but never switched on. */
function buildDevices(people, today) {
  const rand = random(seedOf('devices'));
  const numbers = new Set();
  const hostname = (kind) => {
    let n;
    do n = 1 + Math.floor(rand() * 1999); while (numbers.has(`${kind}${n}`));
    numbers.add(`${kind}${n}`);
    return `NW-${kind}-${String(n).padStart(4, '0')}`;
  };
  const device = (person, kind) => ({
    id: uuid(rand),
    hostname: hostname(kind),
    person,
    osVersion: rand() < 0.94 ? '11' : '10',
    developer: person?.department === 'Engineering',
    quietMinutes: rand() < 0.8 ? 1 + Math.floor(rand() * 14) : 60 + Math.floor(rand() * 420),
    lastSeen: person?.awayDaysAgo ? today - person.awayDaysAgo * DAY + (9 + rand() * 8) * HOUR : null,
    neverReported: !person,
  });
  const devices = [];
  for (const person of people) {
    const laptop = device(person, 'LAPTOP');
    person.devices.push(devices.length);
    devices.push(laptop);
  }
  for (const person of people.filter((p) => p.department === 'Engineering' && p.ai && !p.awayDaysAgo).slice(0, 54)) {
    person.devices.push(devices.length);
    devices.push(device(person, 'DESK'));
  }
  for (let i = 0; i < 6; i += 1) devices.push(device(null, 'LAPTOP'));
  return devices;
}

/** One prompt a person sent, with what the classifier found in it and what the rules decided. */
function makePrompt(rand, person, k, t) {
  const tool = TOOLS[k];
  const [credential, pii, card, falcon] = RISK[person.department];
  const label = rand() < credential ? 'credential'
    : rand() < pii ? 'customer_pii'
      : rand() < card ? 'payment_card'
        : rand() < falcon ? FALCON.label
          : rand() < (SOURCE[tool.key] ?? 0) * (person.department === 'Engineering' ? 1 : 0.1) ? 'source_code' : null;
  // A rule on labels acts only where the prompt is classified before it is sent: a coding agent's
  // hook. In the browser the labels are found after the prompt has gone, and the prompt is logged.
  const rule = tool.routes.includes('tool.hook')
    ? RULES.find((r) => r.match.labels?.includes(label) && (!r.match.tools || r.match.tools.includes(tool.key))) ?? null
    : null;
  const [low, high] = tool.bytes;
  return {
    t,
    p: person.index,
    k,
    label,
    rule: rule?.rule_id ?? null,
    action: rule ? OUTCOME[rule.action] : 'logged',
    sensitive: rule !== null,
    bytes: Math.round(low + rand() * (high - low) * (rand() < 0.1 ? 3 : 1)),
    att: rand() < (k <= 3 ? 0.05 : 0.01) ? 1 + (rand() < 0.2 ? 1 : 0) : 0,
    dev: person.devices.length > 1 && rand() < 0.35 ? person.devices[1] : person.devices[0],
  };
}

/** Every prompt of the history, oldest first. From the redirect on, ChatGPT is tried less each day and Copilot takes its prompts. */
function simulate(people, today) {
  const prompts = [];
  const redirectAt = workdaysBack(today, REDIRECT_WORKDAYS_AGO) + REDIRECT_HOUR * HOUR;
  const chatgpt = TOOL_INDEX.get('chatgpt_web');
  const copilot = TOOL_INDEX.get('microsoft_copilot');
  for (let back = HISTORY_DAYS - 1; back >= 0; back -= 1) {
    const day = today - back * DAY;
    const weekend = isWeekend(day);
    for (const person of people) {
      if (!person.ai || back > person.startDaysAgo) continue;
      const away = person.awayDaysAgo === null ? Infinity : today - person.awayDaysAgo * DAY;
      const rand = random(seedOf(person.subject, day));
      if (rand() >= (weekend ? person.activity * 0.12 : person.activity)) continue;
      const n = 1 + poisson(rand, person.rate);
      for (let i = 0; i < n; i += 1) {
        const t = day + weighted(rand, weekend ? WEEKEND_HOURS : WEEKDAY_HOURS) * HOUR + Math.floor(rand() * HOUR);
        if (t >= away) continue;
        let k = TOOL_INDEX.get(person.tools[weighted(rand, person.weights)]);
        if (k === chatgpt && t >= redirectAt) {
          if (rand() < 0.6 * Math.exp(-(t - redirectAt) / (1.5 * DAY))) {
            const attempt = makePrompt(rand, person, chatgpt, t - 40_000);
            prompts.push({ ...attempt, label: null, rule: 'redirect_chatgpt_to_copilot', action: 'blocked', sensitive: false, att: 0 });
          }
          k = copilot;
        }
        prompts.push(makePrompt(rand, person, k, t));
      }
    }
  }
  return prompts.sort((a, b) => a.t - b.t);
}

let cached = null;

/** The world for the current UTC day, built once a day. */
function world(now) {
  const today = dayOf(now);
  if (cached?.today === today) return cached;
  const people = buildPeople();
  const devices = buildDevices(people, today);
  const prompts = simulate(people, today);
  const times = Float64Array.from(prompts, (x) => x.t);
  const events = buildEvents(prompts, people, devices);
  cached = { today, people, devices, prompts, times, events, findings: buildFindings(events, today), audit: buildAudit(today, events, people) };
  return cached;
}

/** The first index whose time is at or after t. */
function lowerBound(times, t) {
  let lo = 0;
  let hi = times.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (times[mid] < t) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/** The prompts a read over the window counts: those whose native bucket starts in it, sent before now. */
function promptsIn(w, window, bucket, now) {
  const step = bucket === 'hour' ? HOUR : DAY;
  const from = window ? Math.ceil(Date.parse(window.from) / step) * step : now - 7 * DAY;
  const to = Math.min(window ? Math.ceil(Date.parse(window.to) / step) * step : now, now - LAG);
  return w.prompts.slice(lowerBound(w.times, from), lowerBound(w.times, to));
}

/** The start of the bucket a time falls in. Week and month are reductions over day rows. */
function bucketOf(t, size) {
  if (size === 'hour') return Math.floor(t / HOUR) * HOUR;
  const day = dayOf(t);
  if (size === 'week') return day - ((new Date(day).getUTCDay() + 6) % 7) * DAY;
  if (size === 'month') return Date.UTC(new Date(day).getUTCFullYear(), new Date(day).getUTCMonth(), 1);
  return day;
}

/** Prompts counted into cells by a key; a null key leaves a prompt out. */
function cellsOf(prompts, keyOf) {
  const cells = new Map();
  for (const x of prompts) {
    const key = keyOf(x);
    if (key === null) continue;
    let c = cells.get(key);
    if (!c) cells.set(key, (c = { first: x, submissions: 0, people: new Set(), tools: new Set(), bytes: 0, blocked: 0, warned: 0, sensitive: 0, sensitiveBlocked: 0, sensitiveWarned: 0, attachments: 0 }));
    c.submissions += 1;
    c.people.add(x.p);
    c.tools.add(x.k);
    c.bytes += x.bytes;
    c.attachments += x.att;
    if (x.action === 'blocked') c.blocked += 1;
    if (x.action === 'warned') c.warned += 1;
    if (x.sensitive) {
      c.sensitive += 1;
      if (x.action === 'blocked') c.sensitiveBlocked += 1;
      if (x.action === 'warned') c.sensitiveWarned += 1;
    }
  }
  return [...cells.values()];
}

// ── Envelopes ──────────────────────────────────────────────────────────────────────────────

/** The fleet as coverage states it: enrolled devices, and those that reported in the last day. */
function fleet(w) {
  const enrolled = w.devices.length;
  return { enrolled, reporting: w.devices.filter((d) => !d.neverReported && !d.lastSeen).length };
}

function coverageFor(w, window, now) {
  const from = window ? Date.parse(window.from) : now - 7 * DAY;
  const to = window ? Date.parse(window.to) : now;
  const days = Math.max(1, Math.round((to - from) / DAY));
  const { enrolled, reporting } = fleet(w);
  const gaps = (enrolled - reporting) * days;
  return coverage({
    state: reporting < enrolled ? 'partial' : 'complete',
    window: [iso(from).slice(0, 10), iso(to).slice(0, 10)],
    devices_enrolled: enrolled,
    devices_reporting: reporting,
    expected_collector_days: enrolled * days,
    observed_collector_days: reporting * days,
    gap_reasons: gaps > 0 ? { unknown: gaps } : {},
    gaps_total: gaps,
  });
}

let auditSeq = 48_200;

/** An audited read's own entry in the audit trail. */
function audited(now) {
  auditSeq += 1;
  return { entry_id: String(auditSeq), written_at: iso(now) };
}

const cursorAt = (offset) => `${Buffer.from(JSON.stringify({ o: offset })).toString('base64url')}.demo`;
function offsetOf(cursor) {
  try {
    return Number(JSON.parse(Buffer.from(String(cursor).split('.')[0], 'base64url').toString()).o) || 0;
  } catch {
    return 0;
  }
}

/** One page of a list, and the cursor for the next. */
function paged(rows, limit, cursor, now) {
  const offset = cursor ? offsetOf(cursor) : 0;
  const slice = rows.slice(offset, offset + limit);
  const next = offset + limit < rows.length ? cursorAt(offset + limit) : null;
  return { slice, page: { returned: slice.length, next_cursor: next, snapshot_upper_bound: iso(now), newer_events_exist: false } };
}

/**
 * An answer as query-api shapes it. A list of current state with nothing in it is empty; a
 * measurement with nothing in it on partial coverage is a floor, not a zero.
 */
function envelope(w, now, data, { source, aggregate = source, window = null, bucket = null, page = null, audit = false, meta = {}, current = false }) {
  const cov = coverageFor(w, window, now);
  const lastRun = now - LAG;
  return {
    api_version: '1',
    query_version: '1',
    result_state: data.length > 0 ? 'ok' : !current && cov.state === 'partial' ? 'coverage_degraded' : 'empty',
    data,
    ...(page ? { page } : {}),
    freshness: freshness({ aggregate, bucket_size: bucket === 'hour' ? 'hour' : 'day', last_run_at: iso(lastRun), last_complete_bucket: iso(Math.floor(lastRun / HOUR) * HOUR - HOUR), lag_seconds: LAG / 1000 }),
    coverage: cov,
    ...(audit ? { audit: audited(now) } : {}),
    meta: { source, ...(bucket ? { applied_bucket: bucket } : {}), ...meta },
  };
}

/** The template parameters or document fields a read carries. */
function readOf(body) {
  const params = body?.params ?? {};
  return { params, window: params.window ?? body?.window ?? null, bucket: params.bucket ?? body?.bucket ?? null, filters: body?.filters ?? [] };
}

const filterOf = (filters, field, op) => filters.find((f) => f.field === field && f.op === op)?.value;
const nativeOf = (bucket) => (bucket === 'hour' ? 'hour' : 'day');
const sanctionOf = (k) => TOOLS[k].sanction ?? 'unknown';

// ── Usage: tools, teams, people ──────────────────────────────────────────────────────────────

function toolsRanked(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const bucket = params.bucket ?? 'day';
  const tool = params.tool ? toolIndexOf(params.tool) : null;
  const prompts = promptsIn(w, window, nativeOf(bucket), now)
    .filter((x) => (tool === null || x.k === tool) && (!params.sanctioned_state || sanctionOf(x.k) === params.sanctioned_state));
  const cells = cellsOf(prompts, (x) => `${bucketOf(x.t, bucket)}|${x.k}`).map((c) => ({ ...c, bucket: bucketOf(c.first.t, bucket) }));
  cells.sort((a, b) => b.bucket - a.bucket || b.submissions - a.submissions || a.first.k - b.first.k);
  const limit = params.limit ?? 400;
  const data = cells.slice(0, limit).map((c) => {
    const t = TOOLS[c.first.k];
    return {
      bucket: iso(c.bucket), tool: `app:${t.key}`, tool_name: t.name, sanctioned_state: t.sanction,
      submissions: c.submissions, users: c.people.size, bytes_total: c.bytes,
      blocked: c.blocked, warned: c.warned, logged: c.submissions - c.blocked - c.warned,
      sensitive: c.sensitive, sensitive_blocked: c.sensitiveBlocked, sensitive_warned: c.sensitiveWarned,
      sensitive_logged: c.sensitive - c.sensitiveBlocked - c.sensitiveWarned,
      attachments: c.attachments, content_prompts: c.submissions,
    };
  });
  const meta = { measure_semantics: { submissions: 'additive', users: 'exact' }, ...(cells.length > limit ? { truncated: true } : {}) };
  // The people behind the usage travel with a read of every tool, each counted once.
  if (tool === null && !params.sanctioned_state) {
    const used = new Map();
    for (const x of prompts) {
      const was = used.get(x.p) ?? { sanctioned: false, other: false };
      if (TOOLS[x.k].sanction === 'sanctioned') was.sanctioned = true;
      else was.other = true;
      used.set(x.p, was);
    }
    const all = [...used.values()];
    meta.extras = {
      people: [{
        people: used.size,
        people_sanctioned_only: all.filter((u) => u.sanctioned && !u.other).length,
        people_both: all.filter((u) => u.sanctioned && u.other).length,
        people_unsanctioned_only: all.filter((u) => u.other && !u.sanctioned).length,
        directory_people: w.people.length,
        device_people: new Set(w.devices.filter((d) => d.person).map((d) => d.person.index)).size,
      }],
    };
  }
  return envelope(w, now, data, { source: 'mart.v_tool_usage', aggregate: 'mart.agg_tool_period', window, bucket, audit: true, meta });
}

/** Who used a tool, a row per bucket and person, ordered by tool and person: a list, never a leaderboard. */
function toolUsers(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const bucket = params.bucket ?? 'day';
  const tool = params.tool ? toolIndexOf(params.tool) : null;
  const state = params.sanctioned_state ?? (tool === null ? 'unsanctioned' : null);
  const subject = params.subject ? w.people.find((p) => p.subject === params.subject)?.index ?? -1 : null;
  const prompts = promptsIn(w, window, nativeOf(bucket), now)
    .filter((x) => (tool === null || x.k === tool) && (!state || sanctionOf(x.k) === state) && (subject === null || x.p === subject));
  const cells = cellsOf(prompts, (x) => `${bucketOf(x.t, bucket)}|${x.k}|${x.p}`).map((c) => ({ ...c, bucket: bucketOf(c.first.t, bucket), person: w.people[c.first.p] }));
  cells.sort((a, b) => b.bucket - a.bucket || a.first.k - b.first.k || a.person.subject.localeCompare(b.person.subject));
  const limit = params.limit ?? 500;
  const data = cells.slice(0, limit).map((c) => ({ bucket: iso(c.bucket), tool: `app:${TOOLS[c.first.k].key}`, subject: c.person.subject, submissions: c.submissions, bytes_total: c.bytes }));
  return envelope(w, now, data, { source: 'mart.agg_tool_user_period', aggregate: 'mart.agg_tool_period', window, bucket, audit: true, meta: cells.length > limit ? { truncated: true } : {} });
}

const TEAM_NAMES = new Map(TEAMS.map((team) => [team.id, team.name]));

function teamGrowth(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const bucket = params.bucket ?? 'day';
  const prompts = promptsIn(w, window, nativeOf(bucket), now);
  const team = (x) => w.people[x.p].team;
  const cells = cellsOf(prompts, (x) => (team(x) && (!params.team || team(x) === params.team) ? `${bucketOf(x.t, bucket)}|${team(x)}` : null))
    .map((c) => ({ ...c, bucket: bucketOf(c.first.t, bucket), team: team(c.first) }));
  cells.sort((a, b) => b.bucket - a.bucket || b.submissions - a.submissions || a.team.localeCompare(b.team));
  const data = cells.map((c) => ({ bucket: iso(c.bucket), team: c.team, team_name: TEAM_NAMES.get(c.team), submissions: c.submissions, users: c.people.size }));
  const active = new Set(prompts.map((x) => x.p));
  const extras = { team_coverage: [{ users_all: active.size, users_in_teams: [...active].filter((p) => w.people[p].team).length, teams: TEAMS.length }] };
  return envelope(w, now, data, { source: 'mart.agg_team_period', window, bucket, meta: { extras } });
}

/** The people behind the teams named, a row per day, team and person. */
function teamMembers(body) {
  const now = Date.now();
  const w = world(now);
  const { window, filters } = readOf(body);
  const teams = new Set(filterOf(filters, 'team', 'in') ?? []);
  const prompts = promptsIn(w, window, 'day', now).filter((x) => teams.has(w.people[x.p].team));
  const cells = cellsOf(prompts, (x) => `${dayOf(x.t)}|${x.p}`).map((c) => ({ ...c, person: w.people[c.first.p] }));
  cells.sort((a, b) => b.first.t - a.first.t || a.person.team.localeCompare(b.person.team) || a.person.subject.localeCompare(b.person.subject));
  const data = cells.map((c) => ({ bucket: iso(dayOf(c.first.t)), team: c.person.team, team_name: TEAM_NAMES.get(c.person.team), subject: c.person.subject, name: c.person.name, submissions: c.submissions }));
  return envelope(w, now, data, { source: 'mart.v_team_member_period', aggregate: 'mart.agg_user_period', window, bucket: 'day', audit: true });
}

/** The day a person last sent a prompt that has arrived, or null. */
function lastActiveDay(w, person, now) {
  const visible = lowerBound(w.times, now - LAG);
  for (let i = visible - 1; i >= 0; i -= 1) {
    if (w.prompts[i].p === person.index) return iso(dayOf(w.prompts[i].t));
    if (w.prompts[i].t < now - 30 * DAY) break;
  }
  return null;
}

const personRow = (w, person, now) => ({
  subject: person.subject, name: person.name, directory_name: person.name, subject_name: person.email,
  department: person.department, org_unit: null, directory_status: 'active', last_active_day: lastActiveDay(w, person, now),
});

/** The people list, in name order: a name prefix, one reference, or the references named. */
function peopleList(body) {
  const now = Date.now();
  const w = world(now);
  let rows = w.people;
  for (const f of body?.filters ?? []) {
    if (f.field === 'name_key' && f.op === 'starts_with') rows = rows.filter((p) => p.name.toLowerCase().startsWith(String(f.value).toLowerCase()));
    if (f.field === 'subject' && f.op === 'eq') rows = rows.filter((p) => p.subject === f.value);
    if (f.field === 'subject' && f.op === 'in') rows = rows.filter((p) => f.value.includes(p.subject));
    if (f.field === 'directory_status' && f.op === 'in' && !f.value.includes('active')) rows = [];
  }
  const { slice, page } = paged(rows, body?.limit ?? 50, body?.cursor, now);
  return envelope(w, now, slice.map((p) => personRow(w, p, now)), { source: 'mart.v_person', page, audit: true, current: true, meta: { kind: 'list' } });
}

function subjectSeries(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const bucket = params.bucket ?? 'day';
  const person = w.people.find((p) => p.subject === params.subject);
  const prompts = person ? promptsIn(w, window, nativeOf(bucket), now).filter((x) => x.p === person.index) : [];
  const data = cellsOf(prompts, (x) => bucketOf(x.t, bucket)).map((c) => ({
    bucket: iso(bucketOf(c.first.t, bucket)), subject: params.subject, submissions: c.submissions, bytes_total: c.bytes,
    tools_used: c.tools.size, block_events: c.blocked, attachments: c.attachments, content_prompts: c.submissions,
  })).reverse();
  const extras = { flush_check: [{ rows_in_window: prompts.length, late_flush_rows: 0 }], subject_tools: [{ tools: new Set(prompts.map((x) => x.k)).size }] };
  return envelope(w, now, data, { source: 'mart.agg_user_period', window, bucket, audit: true, meta: { extras } });
}

// ── Rules ─────────────────────────────────────────────────────────────────────────────────

const toolNames = (keys) => (keys ? Object.fromEntries(keys.map((key) => [key, TOOLS[TOOL_INDEX.get(key)].name])) : null);

function ruleList() {
  const now = Date.now();
  const w = world(now);
  const data = RULES.map((r, i) => ({ position: i + 1, rule: r.rule_id, action: r.action, match: r.match, label_names: null, tool_names: toolNames(r.match.tools) }));
  return envelope(w, now, data, { source: 'ops.enforcement_rule', current: true });
}

/** The prompts each rule decided, by week (or by hour for a window shorter than a day). */
function ruleMatches(body) {
  const now = Date.now();
  const w = world(now);
  const { window, bucket } = readOf(body);
  const size = bucket ?? 'week';
  const prompts = promptsIn(w, window, nativeOf(size), now);
  const cells = cellsOf(prompts, (x) => (x.rule ? `${bucketOf(x.t, size)}|${x.rule}` : null)).map((c) => ({ ...c, bucket: bucketOf(c.first.t, size) }));
  cells.sort((a, b) => b.bucket - a.bucket || a.first.rule.localeCompare(b.first.rule));
  const data = cells.map((c) => ({ bucket: iso(c.bucket), rule: c.first.rule, matches: c.submissions, blocked: c.blocked, warned: c.warned, logged: c.submissions - c.blocked - c.warned }));
  return envelope(w, now, data, { source: 'mart.agg_rule_period', window, bucket: size });
}

// ── Devices ───────────────────────────────────────────────────────────────────────────────

function deviceRow(d, now) {
  const collectors = d.neverReported ? [] : [...COLLECTORS, ...(d.developer ? DEVELOPER_COLLECTORS : [])].sort();
  const seen = d.neverReported ? null : d.lastSeen ?? now - d.quietMinutes * MINUTE;
  return {
    device: d.id, hostname: d.hostname, device_os: 'windows', os_version: d.osVersion,
    agent_version: d.neverReported ? null : AGENT_VERSION, collection_mode: d.neverReported ? null : 'm3', managed_state: 'managed',
    user_ref: d.person?.subject ?? null, subject_name: d.person?.email ?? null, directory_name: d.person?.name ?? null, region: 'eu',
    liveness: d.neverReported ? 'never_reported' : d.lastSeen ? 'stale' : 'reporting',
    collectors_reporting: collectors.length, collectors_healthy: collectors.length, collectors_degraded: 0, collectors_absent: 0, collectors_tampered: 0,
    collectors, spool_depth: d.neverReported ? null : 0, spool_dropped_total: d.neverReported ? null : 0, last_seen_at: seen === null ? null : iso(seen),
  };
}

const byDeviceId = (w) => [...w.devices].sort((a, b) => a.id.localeCompare(b.id));

function deviceList(body) {
  const now = Date.now();
  const w = world(now);
  const { params } = readOf(body);
  const rows = byDeviceId(w).map((d) => deviceRow(d, now)).filter((r) => (!params.liveness || r.liveness === params.liveness)
    && (!params.device_os || r.device_os === params.device_os) && (!params.managed_state || r.managed_state === params.managed_state)
    && (!params.region || r.region === params.region) && (!params.collector || r.collectors.includes(params.collector))
    && (!params.collector_state || (params.collector_state === 'healthy' && r.collectors_healthy > 0)));
  const { slice, page } = paged(rows, params.limit ?? 50, params.cursor, now);
  const all = w.devices.map((d) => deviceRow(d, now));
  const count = (liveness) => all.filter((r) => r.liveness === liveness).length;
  const extras = { device_status: [{ devices_enrolled: all.length, revoked: 0, never_reported: count('never_reported'), stale: count('stale'), degraded: 0, tampered: 0, reporting: count('reporting') }] };
  return envelope(w, now, slice, { source: 'mart.v_device_liveness', aggregate: 'ops.device', page, current: true, meta: { extras } });
}

function deviceLiveness(body) {
  const now = Date.now();
  const w = world(now);
  const id = filterOf(readOf(body).filters, 'device', 'eq');
  const data = w.devices.filter((d) => d.id === id).map((d) => deviceRow(d, now));
  return envelope(w, now, data, { source: 'mart.v_device_liveness', aggregate: 'ops.device', current: true, meta: { kind: 'list' } });
}

function collectorStates(body) {
  const now = Date.now();
  const w = world(now);
  const id = filterOf(readOf(body).filters, 'device', 'eq');
  const data = w.devices.filter((d) => d.id === id).flatMap((d) => {
    const row = deviceRow(d, now);
    return row.collectors.map((collector) => ({ device: d.id, collector, collector_state: 'healthy', error_code: null, last_report_at: row.last_seen_at, last_success_at: row.last_seen_at }));
  });
  return envelope(w, now, data, { source: 'ops.collector_state', current: true, meta: { kind: 'list' } });
}

// ── Events, findings and their content ──────────────────────────────────────────────────────

// What people typed, by what the classifier found in it and, for the rest, by department or by
// coding tool. Every name, address and number is invented; secrets are placeholders.
const TEXTS = Object.freeze({
  credential: [
    'Why does the deploy script fail with AccessDenied? This is the profile it uses:\n\n[default]\naws_access_key_id = AKIAXXXXXXXXXXXXXXXX\naws_secret_access_key = XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\nregion = eu-west-2',
    'The nightly SFTP upload to the warehouse stopped after the server move. Is anything wrong with this key?\n\n-----BEGIN PRIVATE KEY-----\nXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\nXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\n-----END PRIVATE KEY-----',
  ],
  customer_pii: [
    'Rewrite this reply to a customer who says the date of birth on her account is wrong. Account holder: R. Whitfield, date of birth 14/03/1986, home address 22 Larch Close, Leeds. Keep it apologetic and under 120 words.',
    'Summarise this customer complaint and list the next steps. Customer: A. Kowal, home address 7 Mill Lane, Bristol. Order 55102 arrived damaged twice and he wants a full refund.',
    'Turn these call notes into a CRM update. The customer asked us to change her home address to 3 Orchard Row, York and confirmed her date of birth for verification.',
    'Draft an email asking the customer for the passport number and date of birth we need to finish the credit check on the new account.',
    'Format this list of new starters for payroll with columns for name, national insurance number, home address and start date.',
    'Write a short apology to J. Okonkwo (mobile 07700 900412) whose delivery to 18 Station Road, Derby was left with a neighbour without permission.',
    'Check this customer record for mistakes before I send it to the courier: M. Lindqvist, 41 Harbour Street, Hull, phone 07700 900187, date of birth 02/11/1979.',
    'Group these five customer complaints by cause. Each has the customer name, email and order number; I only need the themes back.',
    'Draft the letter confirming the change of bank details for our customer P. Navarro, account ending 4471, effective from the next invoice.',
    'Write a reference letter for a former employee, T. Brennan, home address 9 Kiln Way, Norwich, who worked here from 2019 to 2024.',
  ],
  payment_card: [
    'A customer was charged twice for the same order. Card 4111 1111 1111 1111, two payments of 249.00 on consecutive days. Draft the refund request for the finance queue.',
    'Write the chargeback response for this dispute. Card 5555 5555 5555 4444, order 77310 delivered on the 3rd and signed for at the customer address.',
  ],
  source_code: [
    'Why does this throw on an empty list?\n\nfunction totalOutstanding(invoices) {\n  return invoices.map((i) => i.amount).reduce((a, b) => a + b);\n}',
    'Add retry with exponential backoff to this client:\n\nimport requests\n\ndef fetch_orders(customer_id):\n    r = requests.get(f"{BASE_URL}/customers/{customer_id}/orders", timeout=5)\n    r.raise_for_status()\n    return r.json()',
    'Write unit tests for this class, including rounding half-up at two decimals:\n\npublic class InvoiceRounding {\n    public static BigDecimal round(BigDecimal value) {\n        return value.setScale(2, RoundingMode.HALF_UP);\n    }\n}',
    'This query is slow on the shipments table. How would you rewrite it?\n\nSELECT * FROM shipments s\nWHERE lower(s.postcode) LIKE \'%ls1%\'\nORDER BY s.created_at DESC;',
    'Convert this callback to async/await and keep the error handling:\n\nfs.readFile(path, (err, data) => {\n  if (err) return done(err);\n  done(null, parseManifest(data));\n});',
    'Review this Terraform for the reporting storage account and tell me what is missing:\n\nresource "azurerm_storage_account" "reports" {\n  name                     = "nwreports"\n  account_tier             = "Standard"\n  account_replication_type = "LRS"\n}',
  ],
  [FALCON.label]: [
    'Summarise the Project Falcon board update into five bullets for the leadership offsite. Leave the valuation range out.',
    'Draft a holding statement we can use if Project Falcon leaks before the announcement.',
    'List the questions Halden Freight is likely to ask in the Falcon deal diligence call, grouped by finance, legal and operations.',
    'Tighten the wording of slide 4 of falcon-board-pack.pptx so it fits on one slide.',
  ],
  coding: [
    'Refactor the order export job so it streams rows instead of loading the whole month into memory.',
    'Fix the flaky test in checkout_spec and explain what was causing it.',
    'Add pagination to the /customers endpoint and update the OpenAPI spec to match.',
    'Write a migration that adds an index on invoices (customer_id, due_date) without locking the table.',
    'Explain what this stack trace means and where to start looking: NullReferenceException in ShipmentPlanner.Allocate.',
    'Rename OrderStatus.Pending to AwaitingPayment across the solution and update the tests.',
    'Add structured logging to the warehouse sync worker, one line per batch with its size and duration.',
    'Write a GitHub Actions workflow that runs the integration tests against a Postgres service container.',
  ],
  Engineering: [
    'Compare Postgres logical replication and Debezium for streaming order changes to the data warehouse.',
    'Write a short design note on retiring the legacy invoice PDF service.',
    'Explain the difference between a liveness and a readiness probe, with an example for a queue worker.',
    'Draft the incident summary for yesterday\'s checkout outage from these timeline notes.',
  ],
  'Customer Success': [
    'Write a friendly follow-up to a customer whose ticket has been open for nine days, explaining that the fix ships on Monday.',
    'Summarise this customer call transcript into issues, sentiment and next steps.',
    'Suggest three ways to cut first-response time in our customer support queue without adding headcount.',
    'Draft a renewal check-in email for a customer who has not logged in for a month.',
    'Turn our returns policy into five short answers for the help centre.',
    'Write a macro reply for customers asking when the new tracking page goes live.',
  ],
  Sales: [
    'Draft a short first email to an operations director at a logistics company about cutting returns processing time.',
    'Turn these discovery call notes into a mutual action plan with owners and dates.',
    'What questions should I ask a customer procurement team before a renewal negotiation?',
    'Write three follow-up emails for a prospect who went quiet after the pricing call.',
    'Summarise the main objections from this quarter\'s lost deals into a one-page brief.',
  ],
  Marketing: [
    'Give me ten subject lines for the November newsletter about our winter range.',
    'Rewrite this landing page introduction so it is shorter and more concrete.',
    'Outline a customer case study from these interview notes, with a headline and three sections.',
    'Suggest a LinkedIn post announcing our new Rotterdam warehouse, under 80 words.',
    'Write alt text for these six product photos.',
  ],
  Finance: [
    'Explain the difference between accrued and deferred revenue with a simple example.',
    'Write an Excel formula that flags invoices more than 60 days overdue.',
    'Summarise the variance between the Q3 forecast and actuals from the table below.',
    'Draft the reminder to budget holders that Q4 forecasts are due on Friday.',
  ],
  Legal: [
    'Explain this limitation of liability clause in plain English.',
    'Compare these two NDA templates and list every difference that matters.',
    'Summarise what changes for us in the updated supplier code of conduct.',
  ],
  People: [
    'Draft interview questions for a senior data analyst role.',
    'Write a short welcome message for new starters joining on Monday.',
    'Rewrite the hybrid working policy summary so it reads more clearly.',
    'Suggest a structure for the quarterly engagement survey results presentation.',
  ],
  Operations: [
    'Create a checklist for opening a new warehouse site.',
    'Turn this incident timeline into a short post-incident summary for the leadership team.',
    'Draft a reminder to all staff about the laptop restart on Friday evening.',
    'Compare these three forklift leasing quotes in a table.',
    'Write the shift handover template for the night team.',
  ],
  other: [
    'Turn these meeting notes into action items with owners and dates.',
    'Summarise this supplier proposal in five bullet points.',
    'Draft a polite reply declining the conference invitation.',
  ],
});
const CODING = new Set(['claude_code', 'codex', 'cursor']);

/** The share of each kind of prompt kept as an event in the sample Search reads. */
function keepChance(x, person) {
  if (x.sensitive) return 0.18;
  if (x.label) return 0.25;
  if (x.rule) return 0.02;
  return person.cast ? 0.025 : 0.0018;
}

function buildEvents(prompts, people, devices) {
  const events = [];
  for (const x of prompts) {
    const person = people[x.p];
    const rand = random(seedOf('event', person.subject, x.t));
    if (rand() >= keepChance(x, person)) continue;
    const tool = TOOLS[x.k];
    const pool = TEXTS[x.label] ?? (CODING.has(tool.key) ? TEXTS.coding : TEXTS[person.department] ?? TEXTS.other);
    const variant = Math.floor(rand() * pool.length);
    const routes = tool.routes.length > 1 && tool.key !== 'claude_code' && rand() < 0.6 ? tool.routes.slice(0, 1) : tool.routes;
    const text = pool[variant];
    events.push({
      submissionId: uuid(rand),
      observations: routes.map(() => uuid(rand)),
      t: x.t,
      received: x.t + 2000 + Math.floor(rand() * 20_000),
      prompt: x,
      person,
      device: devices[x.dev],
      tool,
      routes,
      variant,
      text,
      // The private key's prompt is still held on its device: Search cannot find it.
      contentState: x.label === 'credential' && variant === 1 ? 'local_only' : 'uploaded',
      size: Buffer.byteLength(text) + (CODING.has(tool.key) ? x.bytes : 0),
      review: rand(),
    });
  }
  return events.sort((a, b) => b.received - a.received);
}

function eventRow(e) {
  const x = e.prompt;
  return {
    submission_id: e.submissionId, received_at: iso(e.received), first_occurred_at: iso(e.t), last_occurred_at: iso(e.t + (e.routes.length > 1 ? 900 : 0)),
    subject: e.person.subject, subject_name: e.person.email, directory_name: e.person.name,
    tool: `app:${e.tool.key}`, tool_name: e.tool.name, device: e.device.id, provider: null, department: e.person.department,
    mode: 'm3', action: x.action, policy_rule_id: x.rule, content_state: e.contentState, shredded_reason: null, route: e.routes[0],
    detection_basis: 'prompt', prompt_kind: 'user', merge_confidence: 'high', confidence: 'high', observation_count: e.routes.length,
    labels: x.label ? [{ class: x.label, score: SCORE[x.label] }] : [], size_bytes: e.size, winning_fidelity: 'full',
    observed_routes: e.routes, route_count: e.routes.length, attachment_count: x.att,
  };
}

/** Does an event match one filter of the events list? */
function eventMatches(e, row, name, value) {
  if (name === 'tool') return toolIndexOf(value) === TOOL_INDEX.get(e.tool.key);
  if (name === 'class') return row.labels.some((l) => l.class === value);
  if (name === 'attachments') return value === 'yes' ? row.attachment_count > 0 : true;
  if (name === 'route') return row.observed_routes.includes(value);
  if (name === 'routes') return row.observed_routes.length > 1;
  if (name === 'prompt_kind_not') return row.prompt_kind !== value;
  return row[name] === value;
}

const LIST_PARAMS = new Set(['window', 'limit', 'cursor']);

/** The events a list read names, newest received first. */
function eventsFor(w, params, now) {
  const from = params.window ? Date.parse(params.window.from) : -Infinity;
  const to = Math.min(params.window ? Date.parse(params.window.to) : Infinity, now);
  return w.events.filter((e) => e.received >= from && e.received < to).map((e) => [e, eventRow(e)])
    .filter(([e, row]) => Object.entries(params).every(([name, value]) => LIST_PARAMS.has(name) || eventMatches(e, row, name, value)))
    .map(([, row]) => row);
}

function activity(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const { slice, page } = paged(eventsFor(w, params, now), params.limit ?? 50, params.cursor, now);
  return envelope(w, now, slice, { source: 'ingest.submission', window, page, audit: true, meta: { kind: 'list' } });
}

/** A finding for each event a classifier rule flagged; those older than a day have mostly been reviewed. */
function buildFindings(events, today) {
  return events.filter((e) => DETECTORS[e.prompt.label]).map((e) => {
    const [rule, title] = DETECTORS[e.prompt.label][e.prompt.label === 'credential' ? e.variant : 0];
    const state = e.received >= today - DAY ? 'open' : e.review < 0.55 ? 'confirmed' : e.review < 0.68 ? 'disputed' : 'open';
    const reviewed = state !== 'open';
    return {
      submission_id: e.submissionId, detected_at: iso(e.received + 1000), rule, rule_title: title, class: e.prompt.label, severity: SEVERITY[e.prompt.label],
      subject: e.person.subject, subject_name: e.person.email, directory_name: e.person.name, tool: `app:${e.tool.key}`, tool_name: e.tool.name,
      mode: 'm3', decided_locally: true, review_state: state, reviewed_by: reviewed ? (e.review < 0.3 ? ARJUN : DANA) : null,
      reviewed_at: reviewed ? iso(e.received + (18 + e.review * 10) * HOUR) : null, policy_action: e.prompt.action,
    };
  });
}

function findings(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const from = window ? Date.parse(window.from) : -Infinity;
  const to = Math.min(window ? Date.parse(window.to) : Infinity, now);
  const rows = w.findings.filter((r) => Date.parse(r.detected_at) >= from && Date.parse(r.detected_at) < to)
    .filter((r) => Object.entries(params).every(([name, value]) => LIST_PARAMS.has(name) || (name === 'tool' ? toolIndexOf(value) === toolIndexOf(r.tool) : r[name] === value)));
  const { slice, page } = paged(rows, params.limit ?? 50, params.cursor, now);
  return envelope(w, now, slice, { source: 'mart.v_finding', aggregate: 'ingest.submission', window, page, audit: true });
}

const visibleEvent = (w, id, now) => w.events.find((e) => e.submissionId === id && e.received < now) ?? null;

/** One event as a record read answers it: a row per observation, the submission's columns repeated. */
function eventDetail(body) {
  const now = Date.now();
  const w = world(now);
  const e = visibleEvent(w, body?.params?.submission_id, now);
  if (!e) {
    return {
      api_version: '1', query_version: '1', result_state: 'not_found',
      error: { code: 'not_found', message: 'No such record.', detail: { purge_window_unknown: true } },
      freshness: envelope(w, now, [], { source: 'ingest.submission' }).freshness, coverage: coverageFor(w, null, now),
    };
  }
  const x = e.prompt;
  const rule = RULES.find((r) => r.rule_id === x.rule) ?? null;
  const labels = x.label ? [{ class: x.label, score: SCORE[x.label] }] : [];
  const head = {
    submission_id: e.submissionId, received_at: iso(e.received), first_occurred_at: iso(e.t), last_occurred_at: iso(e.t + (e.routes.length > 1 ? 900 : 0)),
    user_ref: e.person.subject, tool_fingerprint: `app:${e.tool.key}`, tool_name: e.tool.name, kind: 'prompt', collection_mode: 'm3',
    policy_action: x.action, policy_rule_id: rule?.rule_id ?? null, decided_locally: true, confidence: 'high', merge_confidence: 'high',
    content_state: e.contentState, shredded_reason: null, size_bytes: e.size, content_digest: `sha256:${createHash('sha256').update(e.text).digest('hex')}`,
    labels, classifier_version: 'c-2026.10', winning_source: e.routes[0], winning_fidelity: 'full', observed_routes: e.routes,
    observation_count: e.routes.length, expires_at: iso(e.received + 365 * DAY),
  };
  const data = e.routes.map((route, i) => ({
    ...head,
    observation_event_id: e.observations[i], observation_source: route, observation_kind: 'prompt', direction: 'egress',
    observation_occurred_at: iso(e.t + i * 900), observation_received_at: iso(e.received + i * 1200), observation_size_bytes: e.size,
    observation_labels: i === 0 ? labels : null, policy_decision: i === 0 && rule ? { rule_id: rule.rule_id, action: x.action } : null,
    detection_basis: 'prompt', window_start: null, window_end: null, submission_count: null, bytes_total: null,
    observation_device: e.device.id, observation_provider: null,
  }));
  return envelope(w, now, data, { source: 'ingest.submission', audit: true, meta: { kind: 'record', content_state: e.contentState } });
}

// ── The audit trail ─────────────────────────────────────────────────────────────────────────

/** A dozen entries from the last week, oldest first, each hashing the one before it. */
function buildAudit(today, events, people) {
  const at = (workdaysAgo, h, m) => workdaysBack(today, workdaysAgo) + h * HOUR + m * MINUTE;
  const caseEvent = events.find((e) => e.prompt.label === 'credential' && e.contentState === 'uploaded' && e.received < at(1, 15, 0)) ?? events[events.length - 1];
  const reviewed = people.find((p) => p.name === 'Mateo Silva');
  const week = (t) => ({ window: { from: iso(dayOf(t) - 7 * DAY), to: iso(t) } });
  // Arjun's reads, so his page of the trail is long enough to read on its own.
  const reads = [
    [4, 9, 15, 'query.aggregate', 'mart.agg_tool_period'], [4, 9, 20, 'query.aggregate', 'mart.agg_team_period'],
    [3, 13, 2, 'content.search', 'search_text'], [3, 13, 4, 'query.events', 'ingest.submission'],
    [3, 16, 20, 'export.list', 'mart.agg_tool_period'], [2, 10, 5, 'query.aggregate', 'mart.agg_tool_period'],
    [2, 14, 50, 'query.events', 'ingest.submission'], [1, 9, 30, 'query.aggregate', 'mart.agg_class_period'],
    [1, 9, 33, 'content.search', 'search_text'], [1, 11, 12, 'query.aggregate', 'mart.agg_team_period'],
    [0, 8, 40, 'query.aggregate', 'mart.agg_tool_period'],
  ].map(([d, h, m, action, objectType]) => [at(d, h, m), ARJUN, action, objectType, null, null, null, week(at(d, h, m))]);
  const entries = [
    ...reads,
    [at(4, 10, 3), DANA, 'query.events', 'ingest.submission', null, reviewed.subject, 'DPO-0117', { ...week(at(4, 10, 3)), filters: { subject: reviewed.subject } }],
    [at(4, 14, 10), ARJUN, 'tool.sanction', 'tool', 'claude_code', null, null, { previous: 'unknown', new: 'sanctioned' }],
    [at(4, 14, 12), ARJUN, 'tool.sanction', 'tool', 'gemini_web', null, null, { previous: 'unknown', new: 'unsanctioned' }],
    [at(4, 16, 45), ARJUN, 'export.list', 'ingest.submission', null, null, null, { ...week(at(4, 16, 45)), filters: { action: 'blocked' }, row_count: 48 }],
    [at(3, 9, 41), DANA, 'tool.sanction', 'tool', 'chatgpt_web', null, null, { previous: 'unknown', new: 'unsanctioned' }],
    [at(3, 9, 52), DANA, 'tenant.enforcement_rules.set', 'tenant', TENANT_ID, null, null, { rules: RULES.length, added: ['redirect_chatgpt_to_copilot'] }],
    [at(2, 11, 30), ARJUN, 'query.aggregate', 'mart.agg_tool_user_period', null, null, null, { ...week(at(2, 11, 30)), tool: 'app:chatgpt_web' }],
    [at(1, 15, 5), DANA, 'query.events', 'ingest.submission', null, caseEvent.person.subject, 'SEC-1042', { ...week(at(1, 15, 5)), filters: { subject: caseEvent.person.subject, class: 'credential' } }],
    [at(1, 15, 6), DANA, 'query.record', 'ingest.submission', caseEvent.submissionId, caseEvent.person.subject, 'SEC-1042', {}],
    [at(1, 15, 6), DANA, 'content.reveal', 'ingest.submission', caseEvent.submissionId, caseEvent.person.subject, 'SEC-1042', { event_ids: caseEvent.observations.slice(0, 1) }],
    [at(0, 8, 47), DANA, 'query.people', 'mart.v_person', null, null, null, { filters: [] }],
    [at(0, 8, 52), ARJUN, 'query.events', 'ingest.submission', null, null, null, { ...week(at(0, 8, 52)), filters: { action: 'blocked' } }],
  ].sort((x, y) => x[0] - y[0]);
  let prev = createHash('sha256').update(TENANT_ID).digest('hex');
  return entries.map(([t, actor, action, objectType, objectId, subject, reference, detail], i) => {
    const row = { audit_seq: 48_120 + i * 7, occurred_at: iso(t), actor_type: 'user', actor, action, object_type: objectType, object_id: objectId, subject, case: reference, detail };
    const hash = createHash('sha256').update(prev + JSON.stringify(row)).digest('hex');
    const entry = { ...row, prev_hash: prev, row_hash: hash };
    prev = hash;
    return entry;
  }).reverse();
}

function auditTrail(body) {
  const now = Date.now();
  const w = world(now);
  const { params, window } = readOf(body);
  const from = window ? Date.parse(window.from) : -Infinity;
  const to = Math.min(window ? Date.parse(window.to) : Infinity, now);
  const rows = w.audit.filter((r) => Date.parse(r.occurred_at) >= from && Date.parse(r.occurred_at) < to)
    .filter((r) => ['actor', 'action', 'object_type', 'subject', 'case'].every((name) => !params[name] || r[name] === params[name]));
  const { slice, page } = paged(rows, params.limit ?? 50, params.cursor, now);
  return envelope(w, now, slice, { source: 'ops.audit', window, page, audit: true, current: true });
}

const QUERY = {
  q1_tools_ranked: toolsRanked,
  q2_unsanctioned_users: toolUsers,
  q3_team_growth: teamGrowth,
  q5_findings: findings,
  q6_subject_series: subjectSeries,
  q7_devices: deviceList,
  q8_activity: activity,
  q9_event_detail: eventDetail,
  q10_audit_trail: auditTrail,
  'mart.v_team_member_period': teamMembers,
  'mart.v_person': peopleList,
  'mart.v_device_liveness': deviceLiveness,
  'ops.collector_state': collectorStates,
  'ops.enforcement_rule': ruleList,
  'mart.agg_rule_period': ruleMatches,
};

/** The answer to a query document, or null to fall back to the dashboard's fixtures. */
export function query(key, body) {
  const answer = QUERY[key];
  return typeof answer === 'function' ? answer(body) : answer ?? null;
}

// ── Admin reads ─────────────────────────────────────────────────────────────────────────────

/** The day the rollout began: the deployment key, the directory sync and the teams date from it. */
const rolloutAt = (now) => dayOf(now) - 104 * DAY + 9 * HOUR;

function settings() {
  return {
    ceiling_mode: 'm3', collection_mode: 'm3', scope_overrides: {},
    event_retention_days: 365, content_retention_days: 30, retention_defaults: { event_days: 90, content_days: 30 },
    content_search: 'full_text',
    tools: CATALOGUE.map(([key, name]) => ({
      tool_key: key, display_name: name, sanctioned_state: TOOLS[TOOL_INDEX.get(key)]?.sanction ?? 'unknown', fingerprints: [`app:${key}`, ...(FINGERPRINTS[key] ?? [])],
    })),
    endpoint: {
      inventory: true, processes: true, flows: true, otel: true, hooks: true, hooks_managed_only: false,
      tools: { claude_code: { otel: true, hooks: true }, codex: { otel: true, hooks: true }, copilot: { otel: true, hooks: false }, cursor: { otel: false, hooks: true }, ollama: { loopback: false } },
    },
    tls_inspection: true, restart_notice: 'dashboard', kill_switches: [],
    data_classes: DATA_CLASSES, app_categories: ['chat_assistant', 'coding_agent', 'ide', 'ide_assistant', 'inference_api', 'local_runtime'],
    custom_labels: [FALCON],
  };
}

function rules() {
  return {
    rules: RULES.map((r) => ({
      rule_id: r.rule_id, action: r.action,
      match: { labels: r.match.labels ?? [], tools: r.match.tools ?? [], categories: [], sanction: [], routes: [] },
      message: r.message, ...(r.link ? { link: r.link } : {}), users: [], enabled: true,
    })),
    users: [],
  };
}

function deployment() {
  const now = Date.now();
  const w = world(now);
  const lastEnrolled = iso(dayOf(now) - 2 * DAY + 14 * HOUR + 22 * MINUTE);
  return {
    connection: { provider: 'entra', status: 'active', entra_tenant_id: ENTRA_TENANT_ID },
    device_verification: 'intune',
    deployment_keys: [{ key_id: 'dk_7f2c91', label: 'Intune: Windows laptops', created_at: iso(rolloutAt(now)), created_by: DANA, expires_at: null, revoked_at: null, enrolment_count: w.devices.length, last_used_at: lastEnrolled }],
    scim: { tokens: [], base_url: null, users: null, groups: null, last_provisioned_at: null },
    devices: { enrolled: w.devices.length, last_enrolled_at: lastEnrolled },
    release: { version: AGENT_VERSION, product_code: '{3B8E5C21-7D4A-4F19-9C62-1E0A8B7D5F34}' },
  };
}

// The directory groups a team or a rule can name, with how many people each holds.
const GROUPS = Object.freeze([
  [ENGINEERING_GROUP.id, ENGINEERING_GROUP.object_id, ENGINEERING_GROUP.name, 310],
  ['grp-customer-success', '5a1e3c7d-92b4-4f08-b6d1-3c8e0f2a7b45', 'Customer Success', 170],
  ['grp-finance', '8c4b2e6f-1a37-4d95-8e20-6f7a1c3d9b02', 'Finance Team', 95],
  ['grp-it-admins', '2d9f6a1b-4c8e-4a73-9d15-0b6e3f8c2a91', 'IT Admins', 4],
  ['grp-legal', '9e7a3d5c-6b12-4f84-a0c9-5d2b8e1f6c37', 'Legal and Compliance', 50],
  ['grp-sales-emea', '4f0c8b2a-7e65-4d31-b9a8-2c6d0e4f8a13', 'Sales EMEA', 128],
  ['grp-sales-na', '6b3d1f9e-2a74-4c58-8f06-9e1a5c7b3d28', 'Sales North America', 82],
]);

function directory() {
  const now = Date.now();
  const w = world(now);
  const done = Math.floor((now - 72 * MINUTE) / MINUTE) * MINUTE;
  return {
    entra_connected: true, graph_available: true, people: w.people.length, groups: 46,
    sync: {
      enabled_by: DANA, enabled_at: iso(rolloutAt(now) - DAY), last_started_at: iso(done - 4 * MINUTE), last_completed_at: iso(done),
      last_status: 'ok', last_error: null, users_synced: w.people.length, groups_synced: 46,
    },
  };
}

function departments() {
  const counts = new Map();
  for (const person of world(Date.now()).people) counts.set(person.department, (counts.get(person.department) ?? 0) + 1);
  return { values: [...counts].sort(([a], [b]) => a.localeCompare(b)).map(([value, people]) => ({ value, people })) };
}

function teams() {
  const created = iso(rolloutAt(Date.now()) + DAY);
  return {
    teams: TEAMS.map((team) => ({
      team_id: team.id, name: team.name,
      includes: [team.name === 'Engineering'
        ? { kind: 'group', group_id: ENGINEERING_GROUP.id, group_object_id: ENGINEERING_GROUP.object_id, name: ENGINEERING_GROUP.name }
        : { kind: 'department', value: team.name, name: team.name }],
      people: 0, members: team.size, created_by: DANA, created_at: created,
    })),
  };
}

// The providers whose own records the product can read, none of them connected.
const PROVIDERS = Object.freeze([
  { provider: 'amazon_bedrock', display_name: 'Amazon Bedrock', vendor: 'amazon', credential: 'intake', intake: 'firehose', source_url: 'https://docs.aws.amazon.com/bedrock/latest/userguide/model-invocation-logging.html', tools: [{ app: 'aws_bedrock', name: 'Amazon Bedrock' }] },
  { provider: 'chatgpt_enterprise', display_name: 'ChatGPT Enterprise', vendor: 'openai', credential: 'api_key', credential_label: 'Compliance API key', account_label: 'Workspace ID', source_url: 'https://cookbook.openai.com/examples/chatgpt/compliance_api/logs_platform', tools: [{ app: 'chatgpt_web', name: 'ChatGPT (web)' }] },
  { provider: 'claude_enterprise', display_name: 'Claude Enterprise', vendor: 'anthropic', credential: 'api_key', credential_label: 'Compliance API key', account_label: 'Organization ID', source_url: 'https://platform.claude.com/docs/en/manage-claude/compliance-api', tools: [{ app: 'claude_web', name: 'Claude (web)' }, { app: 'claude_desktop', name: 'Claude Desktop' }, { app: 'claude_code', name: 'Claude Code' }] },
  { provider: 'gemini_code_assist', display_name: 'Gemini Code Assist', vendor: 'google', credential: 'intake', intake: 'pubsub', source_url: 'https://docs.cloud.google.com/gemini/docs/log-gemini', tools: [{ app: 'gemini_code_assist', name: 'Gemini Code Assist' }, { app: 'gemini_cli', name: 'Gemini CLI' }] },
  { provider: 'microsoft_365_copilot', display_name: 'Microsoft 365 Copilot', vendor: 'microsoft', credential: 'entra_app', source_url: 'https://learn.microsoft.com/en-us/microsoft-365/copilot/extensibility/api/ai-services/interaction-export/aiinteractionhistory-getallenterpriseinteractions', tools: [{ app: 'm365_copilot', name: 'Microsoft 365 Copilot' }] },
  { provider: 'perplexity_enterprise', display_name: 'Perplexity Enterprise', vendor: 'perplexity', credential: 'intake', intake: 'webhook', source_url: 'https://intercom.help/perplexity-ai/en/articles/11652747-audit-logs', tools: [{ app: 'perplexity_web', name: 'Perplexity (web)' }] },
].map((p) => Object.freeze({ credential_label: '', account_label: '', intake: '', ...p, available: true, connection: null })));

const ADMIN = [
  [/^GET \/admin\/v1\/settings$/, settings],
  [/^GET \/admin\/v1\/settings\/rules$/, rules],
  [/^GET \/admin\/v1\/deployment$/, deployment],
  [/^GET \/admin\/v1\/directory$/, directory],
  [/^GET \/admin\/v1\/directory\/groups(\?.*)?$/, () => ({ provisioned: GROUPS.map(([id, objectId, name, members]) => ({ id, object_id: objectId, name, members, directory: 'provisioned' })), directory: [] })],
  [/^GET \/admin\/v1\/directory\/values\?kind=department$/, departments],
  [/^GET \/admin\/v1\/directory\/values\?kind=org_unit$/, () => ({ values: [] })],
  [/^GET \/admin\/v1\/teams$/, teams],
  [/^GET \/admin\/v1\/teams\/[0-9a-f-]{36}\/members$/, () => ({ members: [], limit: 1000 })],
  [/^GET \/admin\/v1\/providers$/, () => ({ entra_connected: true, providers: PROVIDERS })],
  [/^GET \/admin\/v1\/devices\/[0-9a-f-]{36}\/diagnostics$/, () => ({ diagnostics: [] })],
];

/** The answer to an admin request (`METHOD /path?query`), or null. */
export function admin(key) {
  const route = ADMIN.find(([pattern]) => pattern.test(key));
  return route ? { status: 200, body: route[1]() } : null;
}

// ── Content: prompt search, retrieval and the list export ────────────────────────────────────

/** The words of a search, each matching the start of a word in the prompt. */
function searchTerms(query) {
  return String(query ?? '').toLowerCase().split(/[^a-z0-9_]+/).filter(Boolean)
    .map((word) => new RegExp(`\\b${word}\\w*`, 'gi'));
}

/** A one-line extract around the first match, with every match marked. */
function snippetOf(text, terms) {
  const line = text.replace(/\s+/g, ' ');
  const first = Math.min(...terms.map((term) => line.search(term)));
  const start = first > 60 ? line.indexOf(' ', first - 60) + 1 : 0;
  const piece = `${start > 0 ? '…' : ''}${line.slice(start, start + 200)}${start + 200 < line.length ? '…' : ''}`;
  return terms.reduce((s, term) => s.replace(term, (m) => `<em>${m}</em>`), piece);
}

function contentSearch(body) {
  const now = Date.now();
  const w = world(now);
  const terms = searchTerms(body?.query);
  if (terms.length === 0) return { state: 'refused', error: { code: 'search_disabled', message: 'An empty search is refused rather than returning the whole index.' } };
  const from = body?.window?.from ? Date.parse(body.window.from) : -Infinity;
  const to = Math.min(body?.window?.to ? Date.parse(body.window.to) : Infinity, now);
  const hits = w.events.filter((e) => e.contentState === 'uploaded' && e.received >= from && e.received < to
    && (!body.subject || e.person.subject === body.subject) && (!body.tool || toolIndexOf(body.tool) === TOOL_INDEX.get(e.tool.key))
    && (!body.device || e.device.id === body.device) && (!body.mode || body.mode === 'm3')
    && terms.every((term) => e.text.search(term) >= 0))
    .map((e) => ({
      submission_id: e.submissionId, snippet: snippetOf(e.text, terms), rank: 1, unit_kind: 'prompt_body',
      subject: e.person.email, directory_name: e.person.name, device: e.device.id, hostname: e.device.hostname,
      tool: `app:${e.tool.key}`, tool_name: e.tool.name, received_at: iso(e.received),
    }));
  const limit = Number.isInteger(body?.limit) && body.limit > 0 && body.limit <= 50 ? body.limit : 20;
  const offset = body?.cursor ? offsetOf(body.cursor) : 0;
  const next = offset + limit < hits.length ? cursorAt(offset + limit) : null;
  return { state: 'available', hits: hits.slice(offset, offset + limit), truncated: next !== null, next_cursor: next };
}

/** Retrieval URLs minted and not yet redeemed, each good for one read. */
const grants = new Map();

function retrieval(body) {
  const now = Date.now();
  const ids = Array.isArray(body?.event_ids) ? body.event_ids : [];
  const e = world(now).events.find((event) => event.received < now && event.observations.some((id) => ids.includes(id)));
  if (!e || e.contentState !== 'uploaded') {
    return { status: 403, body: { state: 'refused', error: { code: 'no_content_object', message: 'No content object is stored for this event.' } } };
  }
  const grant = uuid(random(seedOf('grant', e.submissionId, now)));
  const url = `/v1/content/retrieval/${TENANT_ID}/${grant}`;
  // A coding agent sends the context it adds around what the person typed; the page shows the typing.
  grants.set(url, e.tool.key === 'claude_code' ? `<system-reminder>\nWorking directory: C:\\src\\orders-service. Branch: main.\n</system-reminder>\n${e.text}` : e.text);
  return {
    status: 200,
    body: {
      state: 'available', event_id: ids[0], grant_id: grant, raw_digest: `sha256:${createHash('sha256').update(e.text).digest('hex')}`,
      expires_at: iso(now + 5 * MINUTE), retrieval_url: url,
    },
  };
}

/** Exports made and not yet downloaded, by download path. */
const exports = new Map();
const EXPORT_SOURCES = Object.freeze({ q8_activity: 'ingest.submission', q5_findings: 'mart.v_finding' });

/** The list on screen as query-api exports it: the same read without its page limit, as CSV. */
function listExport(body) {
  const source = EXPORT_SOURCES[body?.template];
  if (!source) return null;
  const now = Date.now();
  const rows = query(body.template, { ...body, params: { ...body.params, limit: EXPORT_MAX_ROWS, cursor: undefined } }).data;
  const id = uuid(random(seedOf('export', body.template, now)));
  const path = `/v1/export/${id}`;
  exports.set(path, { csv: listExportCsv(source, rows), filename: source === 'mart.v_finding' ? 'findings.csv' : 'events.csv' });
  return {
    status: 200,
    body: { api_version: '1', result_state: 'ok', export: { export_id: id, download_url: path, row_count: rows.length, expires_at: iso(now + EXPORT_TTL_MS) }, audit: null },
  };
}

/**
 * The answer to a content or export request (`METHOD /path`), or null: `{status, body, type?,
 * headers?}`. A retrieval URL and a download are each redeemed once, as the vault's and
 * query-api's are.
 */
export function content(key, body) {
  if (key === 'POST /v1/content-search') return { status: 200, body: contentSearch(body) };
  if (key === 'POST /v1/content/retrieval') return retrieval(body);
  if (key === 'POST /v1/list-export') return listExport(body);
  const [method, path] = key.split(' ');
  if (method !== 'GET') return null;
  if (grants.has(path)) {
    const text = grants.get(path);
    grants.delete(path);
    return { status: 200, body: text, type: 'text/plain; charset=utf-8' };
  }
  if (path.startsWith(`/v1/content/retrieval/${TENANT_ID}/`)) {
    return { status: 410, body: { state: 'no_longer_available', reason: 'grant_already_used', receipt_ref: null } };
  }
  if (exports.has(path)) {
    const { csv, filename } = exports.get(path);
    exports.delete(path);
    return { status: 200, body: csv, type: 'text/csv; charset=utf-8', headers: { 'content-disposition': `attachment; filename="${filename}"` } };
  }
  if (path.startsWith('/v1/export/')) {
    return { status: 410, body: { result_state: 'no_longer_available', error: { code: 'export_used', message: 'The export link was already downloaded once.' } } };
  }
  return null;
}
