// VitePress 1.6 deliberately collapses index.md links and search IDs to a
// directory. Our static contract requires explicit .html destinations instead.
export function explicitIndexLink(href) {
  return typeof href === 'string' && !/^(?:[a-z][\w+.-]*:|\/\/)/i.test(href)
    ? href.replace(/(^|\/)index\.md(?=[?#]|$)/, '$1index.html') : href;
}

export function searchField(document, field) {
  const value = document[field];
  return field === 'id' && typeof value === 'string' ? value.replace(/\/(?=#|$)/, '/index.html') : value;
}

export function explicitMarkdownRoutes(md) {
  const render = md.renderer.rules.link_open;
  md.renderer.rules.link_open = (tokens, index, ...args) => {
    const token = tokens[index];
    const href = token.attrGet('href');
    if (href) token.attrSet('href', explicitIndexLink(href));
    return render(tokens, index, ...args);
  };
}
