import {check} from './lib.mjs';
export function releaseVersion(ref) {
  check(/^refs\/tags\/v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(ref),'stable_release_tag_required');
  return ref.slice('refs/tags/'.length);
}
export function validateEnvironment(env,policies,version) {
  releaseVersion('refs/tags/'+version);
  check(Number.isSafeInteger(env?.id)&&env.name==='docker-release','release_environment_identity');
  check(env.protection_rules?.some(r=>r.type==='required_reviewers'&&r.prevent_self_review===true&&Array.isArray(r.reviewers)&&r.reviewers.length>0&&r.reviewers.every(x=>['User','Team'].includes(x.type)&&Number.isSafeInteger(x.reviewer?.id))),'release_reviewers_required');
  check(env.deployment_branch_policy?.custom_branch_policies===true&&env.deployment_branch_policy.protected_branches===false,'release_tag_policy_required');
  check(policies?.total_count===policies?.branch_policies?.length&&policies.total_count>0,'release_policy_incomplete');
  // Deliberately support only this auditable tag policy (or the exact release tag).
  // More permissive/unrecognized wildcards and all branch policies fail closed.
  check(policies.branch_policies.every(p=>p.type==='tag'&&(p.name==='v*'||p.name===version)),'release_tag_policy_unapproved');
  return {environmentID:env.id,environment:env.name,version,configuredReview:'PASS',actualRunApproval:'NOT_VERIFIED'};
}
const normalServeChecks=['offlineStartup','offlineNoHelper','defaultRemoteOff','defaultNoHelper','sameVolumeRecreation'];
function validateNormalServeProof(proof) {
  check(proof&&normalServeChecks.every(key=>proof[key]==='PASS'),'normal_serve_proof_missing');
}
export function validateNormalServeBenchmark(rows) {
  const summary=rows.at(-1);check(summary?.benchmark==='PASS'&&summary.mode==='serve','normal_serve_benchmark_required');
  const measurements=rows.filter(row=>row.identity);
  check(measurements.length===2&&measurements.every(row=>row.mode==='serve'),'normal_serve_measurements_required');
  for(const row of measurements)validateNormalServeProof(row.normalServe);
  return {benchmarkMode:'serve',normalServe:Object.fromEntries(normalServeChecks.map(key=>[key,'PASS']))};
}
export function validateEvidence(e,ctx) {
  check(e.schema===1&&e.acceptance==='PASS'&&e.benchmark==='PASS'&&e.native===true,'unqualified_evidence');
  check(e.benchmarkMode==='serve','normal_serve_evidence_required');validateNormalServeProof(e.normalServe);
  for(const key of ['sourceRevision','runID','runAttempt','platform'])check(e[key]===ctx[key],'evidence_'+key+'_mismatch');
  check(e.harnessRevision===ctx.sourceRevision,'evidence_harness_mismatch');
}

export async function checkReleaseEnvironment({token,repository,ref,request=fetch}) {
  check(repository==='DevShiftTeam/my-whatsapp-mcp','release_repository');
  const version=releaseVersion(ref);
  const base='https://api.github.com/repos/'+repository+'/environments/docker-release';
  const get=githubReader(token,request);
  return validateEnvironment(await get(base),await get(base+'/deployment-branch-policies?per_page=100'),version);
}
// Supported REST contract: github/rest-api-description@58b1e0c00b39b9e46c24dc4a0ba6a2669c90e9c1.
// Review history has no attempt/time/job/bypass fields. Restrict to the first
// attempt and this workflow's single environment job; never infer a bypass flag.
function githubReader(token,request) {
  return async url=>{
    const response=await request(url,{headers:{Authorization:'Bearer '+token,Accept:'application/vnd.github+json','X-GitHub-Api-Version':'2026-03-10'},signal:AbortSignal.timeout(15000),redirect:'error'});
    check(response.status===200,'release_policy_http_'+response.status);
    check(!response.headers?.get('link'),'release_policy_incomplete');
    return response.json();
  };
}
const positiveID=value=>Number.isSafeInteger(value)&&value>0;
export async function checkCurrentRunApproval({token,repository,ref,runID,runAttempt,sourceRevision,request=fetch}) {
  check(repository==='DevShiftTeam/my-whatsapp-mcp','release_repository');
  const version=releaseVersion(ref);
  check(runAttempt==='1'&&/^[1-9][0-9]*$/.test(runID)&&positiveID(Number(runID))&&/^[a-f0-9]{40}$/.test(sourceRevision),'release_approval_run_context');
  const get=githubReader(token,request),base='https://api.github.com/repos/'+repository;
  const env=await get(base+'/environments/docker-release');
  const configured=validateEnvironment(env,await get(base+'/environments/docker-release/deployment-branch-policies?per_page=100'),version);
  const run=await get(base+'/actions/runs/'+runID);
  check(run?.id===Number(runID)&&run.run_attempt===1&&run.head_sha===sourceRevision&&run.repository?.full_name===repository&&run.event==='push'&&run.path==='.github/workflows/docker.yml'&&positiveID(run.actor?.id)&&positiveID(run.triggering_actor?.id),'release_approval_run_mismatch');
  const rules=env.protection_rules.filter(r=>r.type==='required_reviewers');
  check(rules.length===1&&rules[0].prevent_self_review===true,'release_approval_review_rule');
  const reviewers=rules[0].reviewers.filter(r=>r.type==='User'&&positiveID(r.reviewer?.id)).map(r=>r.reviewer.id);
  const reviews=await get(base+'/actions/runs/'+runID+'/approvals');
  check(Array.isArray(reviews)&&reviews.length===1,'release_approval_ambiguous_or_missing');
  const review=reviews[0];
  check(review?.state==='approved'&&Array.isArray(review.environments)&&review.environments.length===1&&review.environments[0].id===env.id&&review.environments[0].name===env.name,'release_approval_state_environment');
  check(review.user?.type==='User'&&positiveID(review.user.id)&&reviewers.includes(review.user.id)&&review.user.id!==run.actor.id&&review.user.id!==run.triggering_actor.id,'release_approval_reviewer');
  return {...configured,repository,runID,runAttempt,sourceRevision,workflowPath:run.path,reviewerID:review.user.id,actorID:run.actor.id,triggeringActorID:run.triggering_actor.id,actualRunApproval:'INDEPENDENT_REQUIRED_REVIEWER_APPROVAL'};
}
