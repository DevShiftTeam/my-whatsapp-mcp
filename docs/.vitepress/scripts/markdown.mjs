import path from 'node:path';
import matter from 'gray-matter';
import MarkdownIt from 'markdown-it';
import { validateInventory, validateQualification } from './prepare.mjs';
import { guideNotice } from './guide-notice.mjs';

import { DEFAULT_BASE_URL, canonicalURL, markdownURL, validateBase } from './urls.mjs';
export { DEFAULT_BASE_URL, canonicalURL, markdownURL, validateBase } from './urls.mjs';

function rewriteLink(href, { source, baseURL, inventory }) {
  if (href.startsWith('#')) return href;
  if (/^https:\/\//i.test(href)) {
    const url = new URL(href);
    if (url.username || url.password) throw new Error('Unsafe credential-bearing link');
    // Same-site links must also pass the allowlist. External HTTPS links stay exact.
    if (!href.startsWith(baseURL)) return href;
    href = href.slice(baseURL.length);
    return inventoryLink(href, true);
  }
  if (/^[A-Za-z][A-Za-z\d+.-]*:/.test(href) || href.startsWith('//') || href.includes('\\')) throw new Error('Unsafe or private link');
  const sitePath = new URL(baseURL).pathname;
  if (href.startsWith('/')) {
    if (!href.startsWith(sitePath)) throw new Error('Private or excluded absolute link');
    return inventoryLink(href.slice(sitePath.length), true);
  }
  return inventoryLink(href, false);

  function inventoryLink(target, siteRelative) {
    const [name, fragment = ''] = target.split(/#(.*)/s);
    if (name.includes('?')) throw new Error('Unsafe query on public local link');
    let decoded;
    try { decoded = decodeURIComponent(name); } catch { throw new Error('Unsafe encoded link'); }
    if (/[\x00-\x1f\\]/.test(decoded)) throw new Error('Unsafe link');
    const resolved = path.posix.normalize(siteRelative ? decoded : path.posix.join(path.posix.dirname(source), decoded));
    if (resolved === '..' || resolved.startsWith('../') || resolved.startsWith('/')) throw new Error('Private link outside public documentation');
    const page = inventory.pages.find(entry => entry.source === resolved || `${entry.route}.html` === resolved || entry.route === resolved);
    const download = inventory.downloads.find(entry => entry.source === resolved || entry.route === resolved);
    if (!page && !download) throw new Error(`Excluded or unlisted public link: ${target}`);
    const url = page ? canonicalURL(page.route, baseURL) : `${baseURL}${download.route}`;
    return `${url}${fragment ? `#${fragment}` : ''}`;
  }
}

function parserFor(options) {
  const parser = new MarkdownIt({ html: true });
  const normalize = parser.core.ruler.__rules__.find(rule => rule.name === 'normalize').fn;
  parser.core.ruler.at('normalize', state => {
    const rawOffsets = [];
    for (let offset = 0; offset < state.src.length; offset++) {
      rawOffsets.push(offset);
      if (state.src[offset] === '\r' && state.src[offset + 1] === '\n') offset++;
    }
    state.env.rawOffsets = rawOffsets;
    normalize(state);
  });
  // Capture offsets while each leaf block rule still sees the prefix-stripped
  // block state. Outer blockquote/list rules restore those offsets afterward.
  for (const name of ['paragraph', 'heading', 'lheading']) {
    const original = parser.block.ruler.__rules__.find(rule => rule.name === name).fn;
    parser.block.ruler.at(name, (state, startLine, endLine, silent) => {
      const tokenStart = state.tokens.length;
      const matched = original(state, startLine, endLine, silent);
      if (!matched || silent) return matched;
      for (const token of state.tokens.slice(tokenStart)) {
        if (token.type !== 'inline') continue;
        const sourceOffsets = [];
        const contentLines = token.content.split('\n');
        for (let index = 0; index < contentLines.length; index++) {
          const line = token.map[0] + index;
          const contentLine = contentLines[index];
          const leading = /^[ \t]*/.exec(contentLine)[0].length;
          const sourceStart = state.bMarks[line] + state.tShift[line];
          const actual = state.src.slice(sourceStart, state.eMarks[line]);
          const position = actual.indexOf(contentLine.slice(leading));
          if (position < 0) throw new Error('Unsupported inline source mapping');
          for (let character = 0; character < contentLine.length; character++) sourceOffsets.push(state.env.rawOffsets[sourceStart + position + character - leading]);
          if (index < contentLines.length - 1) sourceOffsets.push(state.env.rawOffsets[state.eMarks[line]]);
        }
        token.meta = { ...token.meta, sourceOffsets };
      }
      return matched;
    });
  }
  // Table inline tokens have no map, and markdown-it removes the slash in
  // escaped pipes before inline parsing. Capture each cell while enclosing
  // list/blockquote prefixes are still stripped, retaining per-character offsets.
  const table = parser.block.ruler.__rules__.find(rule => rule.name === 'table').fn;
  parser.block.ruler.at('table', (state, startLine, endLine, silent) => {
    const tokenStart = state.tokens.length;
    const matched = table(state, startLine, endLine, silent);
    if (!matched || silent) return matched;
    let cells = [];
    let cellIndex = 0;
    for (const token of state.tokens.slice(tokenStart)) {
      if (token.type === 'tr_open') {
        const line = token.map[0];
        const start = state.bMarks[line] + state.tShift[line];
        const raw = state.src.slice(start, state.eMarks[line]);
        const leading = raw.length - raw.trimStart().length;
        const row = raw.trim();
        cells = [{ content: '', sourceOffsets: [] }];
        for (let index = 0; index < row.length; index++) {
          const cell = cells.at(-1);
          if (row[index] === '|' && row[index - 1] !== '\\') {
            cells.push({ content: '', sourceOffsets: [] });
          } else {
            if (row[index] === '|') {
              cell.content = cell.content.slice(0, -1);
              cell.sourceOffsets.pop();
            }
            cell.content += row[index];
            cell.sourceOffsets.push(state.env.rawOffsets[start + leading + index]);
          }
        }
        if (cells[0]?.content === '') cells.shift();
        if (cells.at(-1)?.content === '') cells.pop();
        cells = cells.map(cell => {
          const left = cell.content.length - cell.content.trimStart().length;
          const content = cell.content.trim();
          return { content, sourceOffsets: cell.sourceOffsets.slice(left, left + content.length) };
        });
        cellIndex = 0;
      } else if (token.type === 'inline') {
        const cell = cells[cellIndex++] ?? { content: '', sourceOffsets: [] };
        if (cell.content !== token.content) throw new Error('Unsupported table source mapping');
        token.meta = { ...token.meta, sourceOffsets: cell.sourceOffsets };
      }
    }
    return matched;
  });
  const reference = parser.block.ruler.__rules__.find(rule => rule.name === 'reference').fn;
  parser.block.ruler.at('reference', (state, startLine, endLine, silent) => {
    const matched = reference(state, startLine, endLine, silent);
    if (!matched || silent) return matched;
    let content = '';
    const sourceOffsets = [];
    for (let line = startLine; line < state.line; line++) {
      const start = state.bMarks[line] + state.tShift[line];
      const end = state.eMarks[line] + 1;
      content += state.src.slice(start, end);
      for (let offset = start; offset < end; offset++) sourceOffsets.push(state.env.rawOffsets[offset]);
    }
    const prefix = /^\[(?:\\.|[^\]])+\]:\s*/.exec(content);
    if (!prefix) throw new Error('Unsupported reference source mapping');
    const start = prefix[0].length;
    const destination = parser.helpers.parseLinkDestination(content, start, content.length);
    if (!destination.ok) throw new Error('Unsupported reference source mapping');
    const href = parser.normalizeLink(destination.str);
    const rewritten = rewriteLink(href, options);
    if (rewritten !== href) {
      const bracketed = content[start] === '<';
      const destinationStart = start + (bracketed ? 1 : 0);
      const destinationEnd = destination.pos - (bracketed ? 1 : 0);
      state.env.referenceEdits?.push({ start: sourceOffsets[destinationStart], end: sourceOffsets[destinationEnd - 1] + 1, value: rewritten });
    }
    return matched;
  });
  // Validate all parsed destinations ourselves, including schemes markdown-it
  // would otherwise leave as unparsed prose rather than reject.
  parser.validateLink = () => true;
  for (const name of ['link', 'image']) {
    const original = parser.inline.ruler.__rules__.find(rule => rule.name === name).fn;
    parser.inline.ruler.at(name, (state, silent) => {
      const start = state.pos;
      const tokenStart = state.tokens.length;
      const matched = original(state, silent);
      if (!matched || silent) return matched;
      const token = state.tokens.slice(tokenStart).find(item => item.type === (name === 'link' ? 'link_open' : 'image'));
      if (!token) return matched;
      const href = token.attrGet(name === 'link' ? 'href' : 'src');
      const rewritten = rewriteLink(href, options);
      const labelEnd = parser.helpers.parseLinkLabel(state, start + (name === 'image' ? 1 : 0), false);
      let cursor = labelEnd + 1;
      while (/\s/.test(state.src[cursor] || '') && cursor < state.src.length) cursor++;
      if (state.src[cursor] === '(') {
        cursor++;
        while (/\s/.test(state.src[cursor] || '') && cursor < state.src.length) cursor++;
        const destination = parser.helpers.parseLinkDestination(state.src, cursor, state.posMax);
        if (destination.ok && rewritten !== href) {
          const bracketed = state.src[cursor] === '<';
          state.env.linkEdits?.push({ start: cursor + (bracketed ? 1 : 0), end: destination.pos - (bracketed ? 1 : 0), value: rewritten });
        }
      }
      return matched;
    });
  }
  const autolink = parser.inline.ruler.__rules__.find(rule => rule.name === 'autolink').fn;
  parser.inline.ruler.at('autolink', (state, silent) => {
    const start = state.pos;
    const tokenStart = state.tokens.length;
    const matched = autolink(state, silent);
    if (matched && !silent) {
      const token = state.tokens.slice(tokenStart).find(item => item.type === 'link_open');
      if (token) {
        const href = token.attrGet('href');
        const rewritten = rewriteLink(href, options);
        if (rewritten !== href) state.env.linkEdits?.push({ start: start + 1, end: state.pos - 1, value: rewritten });
      }
    }
    return matched;
  });
  return parser;
}

function applyEdits(input, edits) {
  for (const edit of edits.sort((a, b) => b.start - a.start)) input = input.slice(0, edit.start) + edit.value + input.slice(edit.end);
  return input;
}

function stableRecord(value) {
  if (Array.isArray(value)) return value.map(stableRecord);
  if (value && typeof value === 'object') return Object.fromEntries(Object.keys(value).sort().map(key => [key, stableRecord(value[key])]));
  return value;
}

/** Export reviewed public Markdown without executing frontmatter or discarding instructions. */
export function exportPage({ source, route, title, markdown, baseURL = DEFAULT_BASE_URL, qualification, inventory = { pages: [{ source, route, title }], downloads: [] } }) {
  validateBase(baseURL, true);
  validateInventory(inventory.pages, inventory.downloads);
  validateQualification(qualification);
  if (!inventory.pages.some(page => page.source === source && page.route === route)) throw new Error('Page is outside public inventory');
  if (typeof title !== 'string' || !title.trim() || /[\r\n]/.test(title) || typeof markdown !== 'string') throw new Error('Invalid public Markdown title/source');
  const header = /^---([^\r\n]*)\r?\n/.exec(markdown);
  if (header && !['', 'yaml', 'yml'].includes(header[1].trim())) throw new Error('Only non-executable YAML frontmatter is permitted');
  const { content, data } = matter(markdown, { engines: { javascript: { parse() { throw new Error('Executable frontmatter is forbidden'); } } } });
  if (Object.hasOwn(data, 'qualification')) {
    validateQualification(data.qualification);
    if (JSON.stringify(stableRecord(data.qualification)) !== JSON.stringify(stableRecord(qualification))) throw new Error('Staged qualification differs from inventory');
  }
  const options = { source, baseURL, inventory };
  const parser = parserFor(options);
  const initialTokens = parser.parse(content, {});
  const fences = new Map(initialTokens.filter(token => token.type === 'fence' || token.type === 'code_block').map(token => [token.map[0], token]));
  const lines = content.split('\n');
  const converted = [];
  const containers = [];
  for (let line = 0; line < lines.length; line++) {
    const fence = fences.get(line);
    if (fence) {
      const label = /^(\S*)\s+\[([^\]]+)\]\s*$/.exec(fence.info);
      if (fence.type === 'fence' && label && containers.includes('code-group')) {
        converted.push(`### ${label[2]}`, '');
        converted.push(lines[line].replace(/\s+\[[^\]]+\]\s*$/, ''));
        converted.push(...lines.slice(line + 1, fence.map[1]));
      } else converted.push(...lines.slice(line, fence.map[1]));
      line = fence.map[1] - 1;
      continue;
    }
    const opening = /^\s*:::\s*([\w-]+)(?:\s+(.*))?\s*$/.exec(lines[line]);
    if (opening) {
      const [, kind, label] = opening;
      if (!['code-group', 'tip', 'note', 'info', 'warning', 'danger', 'details'].includes(kind)) throw new Error(`Unsupported instruction container: ${kind}`);
      containers.push(kind);
      if (kind !== 'code-group') converted.push(`> **${label || kind[0].toUpperCase() + kind.slice(1)}**`, '');
    } else if (/^\s*:::\s*$/.test(lines[line])) {
      if (!containers.length) throw new Error('Unsupported unmatched instruction container');
      containers.pop();
      converted.push('');
    } else if (/^\s*<PageMarkdown\s*\/>\s*$/.test(lines[line])) {
      // This footer only exposes exports of the current page; it has no procedure.
    } else converted.push(lines[line]);
  }
  if (containers.length) throw new Error('Unsupported unclosed instruction container');
  let body = converted.join('\n');
  const blockEnv = { referenceEdits: [] };
  const tokens = parser.parse(body, blockEnv);
  const bodyLines = body.split('\n');
  const edits = [...blockEnv.referenceEdits];
  const offset = line => bodyLines.slice(0, line).reduce((total, value) => total + value.length + 1, 0);
  let removedTitle = false;
  for (let index = 0; index < tokens.length; index++) {
    const token = tokens[index];
    if (token.type === 'html_block' || token.children?.some(child => child.type === 'html_inline')) throw new Error('Unrecognized Vue/HTML instruction component');
    if (token.type === 'heading_open' && token.tag === 'h1') {
      const [start, end] = token.map;
      if (!removedTitle) {
        edits.push({ start: offset(start), end: offset(end), value: '' });
        removedTitle = true;
        index += 2;
        continue;
      }
      // Keep later section titles without duplicating the document title.
      if (/^ {0,3}#\s/.test(bodyLines[start])) {
        const marker = bodyLines[start].indexOf('#');
        edits.push({ start: offset(start) + marker, end: offset(start) + marker, value: '#' });
      } else {
        edits.push({ start: offset(end - 1), end: offset(end), value: `${bodyLines[end - 1].replace(/=/g, '-')}\n` });
      }
    }
    if (token.type === 'inline') {
      const env = { references: {}, linkEdits: [] };
      // Reference destinations have already been validated by the block parse.
      parser.parseInline(token.content, env);
      for (const edit of env.linkEdits) {
        const start = token.meta.sourceOffsets[edit.start];
        const end = token.meta.sourceOffsets[edit.end - 1] + 1;
        if (body.slice(start, end) !== token.content.slice(edit.start, edit.end)) throw new Error('Unsupported link source mapping');
        edits.push({ start, end, value: edit.value });
      }
    }
  }
  body = applyEdits(body, edits);
  const notice = guideNotice(route, qualification);
  const status = notice ? `**${notice.title}**\n\n${notice.text} [See compatibility.](${canonicalURL('compatibility', baseURL)})\n\n` : '';
  return `# ${title}\n\nSource: <${canonicalURL(route, baseURL)}>\n\n${status}${body}${body.endsWith('\n') ? '' : '\n'}`;
}
