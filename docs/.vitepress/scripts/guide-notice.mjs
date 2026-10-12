// Shared wording for the website and its Markdown exports.
export function guideNotice(route, qualification) {
  if (qualification.availability === 'planned') {
    return { title: 'Not available yet', text: 'This option is planned and is not ready to use.' };
  }
  const setupGuide = route === 'setup' || /^(?:install\/|remote\/|clients\/)/.test(route);
  if (!setupGuide || qualification.availability !== 'preview') return null;
  return {
    title: 'Preview setup',
    text: qualification.status === 'documentation_reviewed'
      ? 'These instructions have been reviewed, but the setup has not yet been tested from start to finish.'
      : 'This setup is still in preview. Check compatibility for the platforms and connections that have been tested.'
  };
}
