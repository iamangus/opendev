(() => {
  'use strict';

  const $ = id => document.getElementById(id);
  const state = {
    token: '', epoch: 0, filter: 'all', search: '', offset: 0,
    selected: new URLSearchParams(location.hash.slice(1)).get('job'),
    jobs: [], total: 0, limit: 50, refreshing: false, queued: false,
    timer: null, requests: new Set(), detailSequence: 0,
    detailSnapshot: '',
  };
  const labels = {
    created: 'Created', planning: 'Planning', planned: 'Planned', working: 'Implementing',
    reviewing: 'In review', integrating: 'Integrating', awaiting_ci: 'Awaiting CI',
    ci_remediating: 'Fixing CI', ci_replanning: 'Replanning', holistic_reviewing: 'Final review',
    ready_to_publish: 'Ready to publish', published: 'Published', no_changes: 'No changes',
    failed: 'Failed', paused: 'Paused', cancelled: 'Cancelled', blocked: 'Blocked',
    changes_requested: 'Changes requested', approved: 'Approved', integration_eligible: 'Ready to integrate',
    integrated: 'Integrated', pending: 'Pending', passed: 'Passed', success: 'Passed',
    failure: 'Failed', completed: 'Completed', queued: 'Queued', in_progress: 'Running',
    started: 'Job started', pr_opened: 'Pull request opened', merged: 'Pull request merged',
    ci_pending: 'CI pending', ci_passed: 'CI passed', ci_failed: 'CI failed',
    ci_blocked: 'CI blocked', foundation: 'Waiting on foundation',
  };
  const label = status => labels[status] || String(status || 'Unknown').replaceAll('_', ' ');
  const tone = status => {
    if (['published', 'integrated', 'approved', 'passed', 'success', 'merged'].includes(status)) return 'green';
    if (['failed', 'failure', 'blocked', 'ci_failed', 'ci_blocked'].includes(status)) return 'red';
    if (['paused', 'changes_requested', 'awaiting_ci', 'pending', 'foundation', 'ci_remediating'].includes(status)) return 'amber';
    if (['cancelled', 'no_changes', 'planned', 'created'].includes(status)) return 'muted';
    return 'blue';
  };
  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }
  function badge(status, text) {
    const node = element('span', 'badge', text || label(status));
    node.dataset.tone = tone(status);
    return node;
  }
  function safeLink(url, text, className = '') {
    try {
      const parsed = new URL(url);
      if (!['https:', 'http:'].includes(parsed.protocol) || parsed.username || parsed.password) return null;
      const node = element('a', className, text);
      node.href = parsed.href;
      node.target = '_blank';
      node.rel = 'noopener noreferrer';
      return node;
    } catch { return null; }
  }
  function date(value) {
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? '—' : parsed.toLocaleString(undefined, {dateStyle: 'medium', timeStyle: 'short'});
  }
  function age(value) {
    const seconds = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
    if (!Number.isFinite(seconds)) return '—';
    if (seconds < 60) return 'just now';
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
    return `${Math.floor(seconds / 86400)}d ago`;
  }
  function progress(done, total) {
    const node = element('progress');
    node.max = Math.max(total, 1);
    node.value = done;
    node.setAttribute('aria-label', `${done} of ${total} tasks integrated or unchanged`);
    return node;
  }
  function notice(title, text, error = false) {
    const node = element('div', error ? 'notice error-notice' : 'notice');
    if (title) node.append(element('p', 'section-label', title));
    node.append(element('p', '', text));
    return node;
  }
  function section(title, extra) {
    const node = element('section', 'detail-section');
    const heading = element('div', 'section-top');
    heading.append(element('h3', 'section-label', title));
    if (extra) heading.append(extra);
    node.append(heading);
    return node;
  }
  function list(items) {
    const node = element('ul', 'list');
    for (const item of items || []) node.append(element('li', '', item));
    return node;
  }
  function placeholder(title, text) {
    state.detailSnapshot = '';
    delete $('detail').dataset.jobId;
    const node = element('div', 'detail-placeholder');
    node.append(element('span', 'detail-symbol', '↗'), element('h2', '', title), element('p', 'muted', text));
    $('detail').replaceChildren(node);
  }
  function displayStatus(job) {
    if (['failed', 'paused', 'published', 'cancelled', 'no_changes'].includes(job.status)) return job.status;
    if (job.tasks_blocked > 0) return 'blocked';
    if (job.foundation_pending) return 'foundation';
    return job.status;
  }

  async function request(path) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 12000);
    state.requests.add(controller);
    try {
      const response = await fetch(path, {
        headers: {Authorization: `Bearer ${state.token}`, Accept: 'application/json'},
        signal: controller.signal, cache: 'no-store', credentials: 'omit', redirect: 'error',
      });
      if (!response.ok) {
        const error = new Error(response.status === 401 ? 'Token rejected. Reconnect with your OpenDev token.' : `Request failed (${response.status}). Try refreshing.`);
        error.status = response.status;
        throw error;
      }
      return await response.json();
    } finally {
      clearTimeout(timeout);
      state.requests.delete(controller);
    }
  }
  function handleError(error, epoch) {
    if (epoch !== state.epoch || !state.token) return;
    if (error.status === 401) { disconnect(error.message); return; }
    $('load-error').textContent = error.name === 'AbortError' ? 'The request timed out. Retrying automatically; displayed data may be out of date.' : error.message;
    $('load-error').hidden = false;
    $('connection').classList.remove('online');
    $('connection-label').textContent = 'Connection interrupted';
  }
  function schedule() {
    clearTimeout(state.timer);
    if (state.token && !document.hidden) state.timer = setTimeout(refresh, 10000);
  }
  function renderJobs() {
    const focused = document.activeElement?.dataset.jobId;
    const fragment = document.createDocumentFragment();
    let refocus;
    for (const job of state.jobs) {
      const item = element('li');
      const button = element('button', `job-row${job.id === state.selected ? ' selected' : ''}`);
      button.type = 'button';
      button.dataset.jobId = job.id;
      button.setAttribute('aria-pressed', String(job.id === state.selected));
      const top = element('div', 'row-top');
      top.append(element('span', 'repository', job.repository), badge(displayStatus(job)));
      const bottom = element('div', 'row-bottom');
      bottom.append(element('span', '', job.tasks_total ? `${job.tasks_done}/${job.tasks_total} tasks done` : 'No tasks yet'));
      const updated = element('time', '', age(job.updated_at));
      updated.dateTime = job.updated_at;
      updated.title = date(job.updated_at);
      bottom.append(updated);
      button.append(top, element('p', 'job-title', job.title || 'Untitled job'), bottom);
      if (job.tasks_total) button.append(progress(job.tasks_done, job.tasks_total));
      button.addEventListener('click', () => select(job.id, true));
      if (focused === job.id) refocus = button;
      item.append(button);
      fragment.append(item);
    }
    $('jobs').replaceChildren(fragment);
    refocus?.focus({preventScroll: true});
    $('list-empty').hidden = state.jobs.length > 0;
    if (!state.jobs.length) {
      $('list-empty').replaceChildren(element('span', 'empty-icon', '⌘'), element('h3', '', state.search || state.filter !== 'all' ? 'No matching jobs' : 'No coding jobs yet'), element('p', 'muted', state.search || state.filter !== 'all' ? 'Try another search or status filter.' : 'Jobs created through OpenDev’s MCP will appear here.'));
    }
    $('result-count').textContent = `${state.total} ${state.total === 1 ? 'job' : 'jobs'}`;
    $('pagination').hidden = state.total <= state.limit;
    $('previous').disabled = state.offset === 0;
    $('next').disabled = state.offset + state.limit >= state.total;
    $('page-label').textContent = `${state.total ? state.offset + 1 : 0}–${Math.min(state.offset + state.limit, state.total)} of ${state.total}`;
  }

  function renderDetail(job) {
    const snapshot = JSON.stringify(job);
    if (state.detailSnapshot === snapshot && $('detail').dataset.jobId === job.id) return;
    state.detailSnapshot = snapshot;
    const fragment = document.createDocumentFragment();
    const tasks = job.plan?.tasks || [];
    const blocked = tasks.filter(task => task.status === 'blocked');
    const done = tasks.filter(task => ['integrated', 'no_changes'].includes(task.status)).length;
    const heading = element('header', 'detail-head');
    const top = element('div', 'row-top');
    top.append(element('span', 'repository', `${job.repository} / ${job.target_branch}`), badge(displayStatus({...job, tasks_blocked: blocked.length})));
    const title = job.plan?.summary || job.directive || 'Untitled job';
    const titleCharacters = Array.from(title);
    heading.append(top, element('h2', 'detail-title', titleCharacters.length > 220 ? titleCharacters.slice(0, 220).join('') + '…' : title));
    const meta = element('div', 'detail-meta');
    const id = element('span', 'mono', `#${job.id.slice(0, 12)}`);
    id.title = job.id;
    meta.append(id, element('span', '', `Updated ${age(job.updated_at)}`));
    if (job.merge_policy === 'manual') meta.append(badge('paused', 'Manual merge approval'));
    heading.append(meta);
    const pr = safeLink(job.pull_request_url, `Open pull request${job.pull_request_number ? ` #${job.pull_request_number}` : ''} ↗`, 'button');
    if (pr) { const actions = element('div', 'detail-actions'); actions.append(pr); heading.append(actions); }
    fragment.append(heading);

    if (job.failure || blocked.length || job.status === 'paused' || job.foundation_pending) {
      const attention = section('Current status');
      if (job.failure) attention.append(notice('Job failure', job.failure, true));
      else if (job.status === 'paused') attention.append(notice('Progression paused', `Paused during ${label(job.paused_from).toLowerCase()}. Already running agents may finish; results are retained until the job resumes.`));
      else if (job.foundation_pending) attention.append(notice('Repository foundation pending', 'Planning is waiting for the repository foundation checks and merge.'));
      for (const task of blocked) attention.append(notice(task.title || task.key, task.block_reason || 'This task needs operator attention.', true));
      fragment.append(attention);
    }

    const directive = section('Directive');
    directive.append(element('p', 'prose', job.directive));
    fragment.append(directive);

    const work = section('Tasks', element('span', 'muted small', `${done} of ${tasks.length} done`));
    if (tasks.length) work.append(progress(done, tasks.length));
    else work.append(element('p', 'muted small', 'The plan has not produced any tasks yet.'));
    for (const task of tasks) {
      const details = element('details', 'task');
      details.open = task.status === 'blocked';
      details.dataset.taskKey = task.key;
      const title = element('summary');
      title.append(element('span', 'task-arrow', '›'), element('span', 'task-name', task.title || task.key), badge(task.status));
      const body = element('div', 'task-body');
      body.append(element('p', 'prose', task.description));
      const info = element('div', 'task-meta');
      info.append(element('span', 'mono', task.key), element('span', '', `Writer attempts: ${task.writer_attempts || 0}`), element('span', '', `Reviewer attempts: ${task.reviewer_attempts || 0}`));
      if (task.commit_sha) info.append(element('span', 'mono', `Commit ${task.commit_sha.slice(0, 12)}`));
      body.append(info);
      if (task.depends_on?.length) body.append(element('p', 'muted small', `Depends on: ${task.depends_on.join(', ')}`));
      if (task.acceptance_criteria?.length) body.append(list(task.acceptance_criteria));
      if (task.block_reason) body.append(notice('Blocked', task.block_reason, true));
      if (task.no_change_reason) body.append(notice('No changes', task.no_change_reason));
      if (task.latest_review) {
        const review = element('div', 'review-box');
        review.append(badge(task.latest_review.verdict, `Review · ${label(task.latest_review.verdict)}`), element('p', '', task.latest_review.summary));
        if (task.latest_review.findings?.length) review.append(list(task.latest_review.findings));
        body.append(review);
      }
      details.append(title, body);
      work.append(details);
    }
    fragment.append(work);

    const ci = section('Continuous integration', job.ci ? badge(job.ci.state) : null);
    if (!job.ci) ci.append(element('p', 'muted small', 'No CI evidence recorded yet.'));
    else {
      ci.append(element('p', 'muted mono', `Head ${job.ci.head_sha?.slice(0, 12) || '—'}`));
      if (!job.ci.checks?.length) ci.append(element('p', 'muted small', `Waiting for required checks: ${(job.ci.required_checks || []).join(', ') || 'not configured'}.`));
      for (const check of job.ci.checks || []) {
        const row = element('div', 'check');
        const info = element('div');
        info.append(element('p', 'check-name', check.name));
        const link = safeLink(check.details_url, 'View check ↗', 'small');
        if (link) info.append(link);
        if (check.summary) info.append(element('p', 'check-summary muted', check.summary));
        row.append(info, badge(check.conclusion || check.status));
        ci.append(row);
      }
    }
    fragment.append(ci);

    if (job.plan?.risks?.length) { const risks = section('Recorded risks'); risks.append(list(job.plan.risks)); fragment.append(risks); }
    const events = (job.events || []).slice(-20).reverse();
    const activity = section('Recent activity');
    if (!events.length) activity.append(element('p', 'muted small', 'No transition history recorded for this job.'));
    else {
      const timeline = element('ol', 'timeline');
      for (const event of events) {
        const row = element('li');
        const content = element('div');
        const time = element('time', '', date(event.created_at));
        time.dateTime = event.created_at;
        content.append(element('p', 'timeline-title', label(event.type.split(':')[0])), time);
        if (event.summary) content.append(element('p', '', event.summary));
        if (event.error) content.append(element('p', 'event-error', event.error));
        row.append(element('span', 'dot'), content);
        timeline.append(row);
      }
      activity.append(timeline);
    }
    fragment.append(activity);
    // Keep expanded task panels open across background refreshes.
    const opened = new Map([...$('detail').querySelectorAll('details.task')].map(node => [node.dataset.taskKey, node.open]));
    const focusedTask = document.activeElement?.closest('details.task')?.dataset.taskKey;
    const sameJob = $('detail').dataset.jobId === job.id;
    $('detail').replaceChildren(fragment);
    $('detail').dataset.jobId = job.id;
    if (sameJob) for (const node of $('detail').querySelectorAll('details.task')) {
      if (opened.has(node.dataset.taskKey)) node.open = opened.get(node.dataset.taskKey);
      if (focusedTask === node.dataset.taskKey) node.querySelector('summary').focus({preventScroll: true});
    }
  }

  async function loadDetail() {
    if (!state.selected || !state.token) return;
    const id = state.selected, epoch = state.epoch, sequence = ++state.detailSequence;
    $('detail').setAttribute('aria-busy', 'true');
    try {
      const job = await request(`api/jobs/${encodeURIComponent(id)}`);
      if (epoch === state.epoch && id === state.selected && sequence === state.detailSequence) renderDetail(job);
    } catch (error) {
      if (epoch !== state.epoch || id !== state.selected || sequence !== state.detailSequence) return;
      if (error.status === 404) placeholder('Job not found', 'This job ID is not available in the current workspace.');
      else handleError(error, epoch);
    } finally {
      if (sequence === state.detailSequence) $('detail').setAttribute('aria-busy', 'false');
    }
  }
  function select(id, user = false) {
    const changed = state.selected !== id;
    state.selected = id;
    const url = new URL(location.href);
    url.hash = new URLSearchParams({job: id}).toString();
    if (user && changed) history.pushState(null, '', url);
    else history.replaceState(null, '', url);
    if (changed) placeholder('Loading job…', 'Reading the latest durable job state.');
    renderJobs();
    loadDetail();
    if (user && matchMedia('(max-width: 800px)').matches) $('detail').scrollIntoView({behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start'});
  }
  async function refresh() {
    clearTimeout(state.timer);
    if (!state.token || document.hidden) return;
    if (state.refreshing) { state.queued = true; return; }
    state.refreshing = true;
    const epoch = state.epoch;
    const signature = `${state.filter}\n${state.search}\n${state.offset}`;
    $('refresh').disabled = true;
    try {
      const query = new URLSearchParams({filter: state.filter, search: state.search, offset: state.offset, limit: state.limit});
      const result = await request(`api/jobs?${query}`);
      if (epoch !== state.epoch || !state.token || signature !== `${state.filter}\n${state.search}\n${state.offset}`) return;
      state.jobs = result.jobs;
      state.total = result.total;
      state.offset = result.offset;
      $('connect-panel').hidden = true;
      $('disconnect').hidden = false;
      $('search').disabled = false;
      $('auth-error').hidden = true;
      $('load-error').hidden = true;
      $('connection').classList.add('online');
      $('connection-label').textContent = 'Live workspace';
      $('last-updated').textContent = `Updated ${new Date(result.updated_at).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit', second: '2-digit'})} · every 10s`;
      for (const name of ['all', 'active', 'attention', 'completed']) $(`count-${name}`).textContent = result.counts[name];
      renderJobs();
      if (!state.selected && state.jobs.length) select(state.jobs[0].id);
      else if (state.selected) await loadDetail();
    } catch (error) { handleError(error, epoch); }
    finally {
      state.refreshing = false;
      if (epoch === state.epoch) {
        $('connect-button').disabled = false;
        $('connect-button').textContent = 'Connect →';
        $('refresh').disabled = !state.token;
      }
      if (state.queued) { state.queued = false; refresh(); }
      else schedule();
    }
  }
  function disconnect(message = '') {
    state.token = '';
    state.epoch++;
    state.detailSequence++;
    state.queued = false;
    clearTimeout(state.timer);
    for (const controller of state.requests) controller.abort();
    state.jobs = [];
    state.total = 0;
    state.detailSnapshot = '';
    $('token').value = '';
    $('connect-button').disabled = false;
    $('connect-button').textContent = 'Connect →';
    $('connect-panel').hidden = false;
    $('disconnect').hidden = true;
    $('refresh').disabled = true;
    $('search').disabled = true;
    $('load-error').hidden = true;
    $('auth-error').hidden = !message;
    $('auth-error').textContent = message;
    $('connection').classList.remove('online');
    $('connection-label').textContent = 'Not connected';
    $('last-updated').textContent = 'Read-only · refreshes every 10s';
    for (const name of ['all', 'active', 'attention', 'completed']) $(`count-${name}`).textContent = '—';
    $('jobs').replaceChildren();
    $('pagination').hidden = true;
    $('result-count').textContent = '—';
    $('list-empty').hidden = false;
    $('list-empty').replaceChildren(element('span', 'empty-icon', '⌘'), element('h3', '', 'Connect to view jobs'), element('p', 'muted', 'Your token is never saved to browser storage.'));
    placeholder('A clear view of the work.', 'Connect and select a job to inspect its progress.');
    $('detail').setAttribute('aria-busy', 'false');
    $('token').focus();
  }

  $('connect-form').addEventListener('submit', event => {
    event.preventDefault();
    const token = $('token').value.trim().replace(/^Bearer\s+/i, '');
    if (!token) return;
    state.epoch++;
    state.token = token;
    $('token').value = '';
    $('connect-button').disabled = true;
    $('connect-button').textContent = 'Connecting…';
    refresh();
  });
  $('disconnect').addEventListener('click', () => disconnect());
  $('refresh').addEventListener('click', refresh);
  for (const button of document.querySelectorAll('[data-filter]')) button.addEventListener('click', () => {
    state.filter = button.dataset.filter;
    state.offset = 0;
    for (const other of document.querySelectorAll('[data-filter]')) {
      const chosen = other === button;
      other.classList.toggle('selected', chosen);
      other.setAttribute('aria-pressed', String(chosen));
    }
    refresh();
  });
  let searchTimer;
  $('search').addEventListener('input', () => {
    clearTimeout(searchTimer);
    state.search = $('search').value.trim();
    state.offset = 0;
    searchTimer = setTimeout(refresh, 220);
  });
  $('previous').addEventListener('click', () => { state.offset = Math.max(0, state.offset - state.limit); refresh(); });
  $('next').addEventListener('click', () => { state.offset += state.limit; refresh(); });
  document.addEventListener('visibilitychange', () => { if (document.hidden) clearTimeout(state.timer); else refresh(); });
  window.addEventListener('hashchange', () => {
    const id = new URLSearchParams(location.hash.slice(1)).get('job');
    if (id && id !== state.selected) select(id);
  });
})();
