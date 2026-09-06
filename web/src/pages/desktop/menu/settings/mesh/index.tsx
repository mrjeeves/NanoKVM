import { useEffect, useRef, useState } from 'react';
import { Button, Divider, Popconfirm, Tag, Typography } from 'antd';
import { LoaderCircleIcon } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import {
  armSupportApproval,
  decideSupportRequest,
  getHelpStatus,
  getMeshStatus,
  rotateClaimCode,
  unclaimDevice
} from '@/api/mesh.ts';

type MeshMembership = {
  networkId: string;
  fleet: boolean;
  joining: boolean;
};

type HelpStatus = {
  canApprove?: boolean;
  enabled: boolean;
  authorised: boolean;
  expiresAt?: number;
  approvalRemainingSeconds: number;
  pending: Array<{
    technician: string;
    sessionId: string;
    agentName: string;
    verificationCode: string;
  }>;
  grantSeconds: number;
  supportId: string;
};

type MeshStatus = {
  enabled: boolean;
  connected: boolean;
  nodeId: string;
  label: string;
  joiningMesh: string;
  claimable: boolean;
  owner: string;
  fleetName: string;
  attachedTo: string;
  attachedLabel: string;
  meshes: MeshMembership[];
  publicClaims: boolean;
  claimCode?: string;
};

export const Mesh = () => {
  const { t } = useTranslation();

  const [status, setStatus] = useState<MeshStatus>();
  const [help, setHelp] = useState<HelpStatus>();
  const [errMsg, setErrMsg] = useState('');
  const [rotating, setRotating] = useState(false);
  const revision = useRef(0);
  const mutating = useRef(false);
  const [deciding, setDeciding] = useState(false);
  const [remaining, setRemaining] = useState(0);
  const [observedAt, setObservedAt] = useState(performance.now());
  const [now, setNow] = useState(performance.now());
  const seconds = Math.max(0, Math.ceil(remaining - Math.max(0, now - observedAt) / 1000));
  const countdown = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
  function updateHelp(next: HelpStatus) {
    setHelp(next);
    setRemaining(next.approvalRemainingSeconds ?? 0);
    setObservedAt(performance.now());
    setNow(performance.now());
  }
  useEffect(() => {
    const timer = setInterval(() => setNow(performance.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const [resetting, setResetting] = useState(false);

  function resetDevice() {
    if (resetting) return;
    setResetting(true);
    unclaimDevice()
      .then((rsp) => {
        if (rsp.code !== 0) {
          setErrMsg(rsp.msg);
          return;
        }
        setErrMsg('');
        setStatus(rsp.data);
      })
      .catch((err) => {
        setErrMsg(err?.message || t('settings.mesh.queryFailed'));
      })
      .finally(() => setResetting(false));
  }

  function decide(action: () => ReturnType<typeof armSupportApproval>) {
    if (mutating.current) return;
    mutating.current = true;
    revision.current++;
    setDeciding(true);
    action()
      .then((rsp) => {
        if (rsp.code !== 0) {
          setErrMsg(rsp.msg);
          return;
        }
        setErrMsg('');
        updateHelp(rsp.data);
      })
      .catch((err) => setErrMsg(err?.message || t('settings.mesh.queryFailed')))
      .finally(() => {
        mutating.current = false;
        setDeciding(false);
      });
  }

  function rotateCode() {
    if (rotating) return;
    setRotating(true);
    rotateClaimCode()
      .then((rsp) => {
        if (rsp.code !== 0) {
          setErrMsg(rsp.msg);
          return;
        }
        setErrMsg('');
        setStatus(rsp.data);
      })
      .catch((err) => {
        setErrMsg(err?.message || t('settings.mesh.queryFailed'));
      })
      .finally(() => setRotating(false));
  }

  useEffect(() => {
    function getStatus() {
      getMeshStatus()
        .then((rsp) => {
          if (rsp.code !== 0) {
            setErrMsg(rsp.msg);
            return;
          }

          setErrMsg('');
          setStatus(rsp.data);
        })
        .catch((err) => {
          setErrMsg(err?.message || t('settings.mesh.queryFailed'));
        });
    }

    let helpReading = false;
    let disposed = false;
    function getHelp() {
      if (helpReading || mutating.current) return;
      helpReading = true;
      const version = revision.current;
      getHelpStatus()
        .then((rsp) => {
          if (disposed || revision.current !== version) return;
          if (rsp.code === 0) updateHelp(rsp.data);
          else setHelp(undefined);
        })
        .catch(() => {
          if (!disposed && revision.current === version) setHelp(undefined);
        })
        .finally(() => {
          helpReading = false;
        });
    }

    getStatus();
    getHelp();

    const interval = setInterval(() => {
      getStatus();
      getHelp();
    }, 5000);
    return () => {
      disposed = true;
      clearInterval(interval);
    };
  }, [t]);

  const attached = status?.attachedLabel || status?.attachedTo;

  return (
    <>
      <div className="text-base">{t('settings.mesh.title')}</div>
      <Divider className="opacity-50" />

      {!status ? (
        <div className="flex w-full items-center justify-center space-x-2 pt-5 text-neutral-500">
          <LoaderCircleIcon className="animate-spin" size={18} />
          <span>{t('settings.mesh.loading')}</span>
        </div>
      ) : !status.enabled ? (
        <div className="pt-5 text-neutral-400">{t('settings.mesh.disabled')}</div>
      ) : (
        <>
          {/* joining mesh */}
          <div className="text-neutral-400">{t('settings.mesh.joiningMesh')}</div>
          <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
            {status.joiningMesh ? (
              <>
                <Typography.Text className="break-all text-center font-mono text-2xl" copyable>
                  {status.joiningMesh}
                </Typography.Text>
                <span className="text-center text-sm text-neutral-400">
                  {t('settings.mesh.joiningMeshDesc')}
                </span>
              </>
            ) : (
              <div className="flex items-center space-x-2 text-neutral-500">
                <LoaderCircleIcon className="animate-spin" size={16} />
                <span>{t('settings.mesh.waiting')}</span>
              </div>
            )}
          </div>
          <Divider className="opacity-50" />

          {/* Support-number requests require a decision or a single-use approval window. */}
          {help?.enabled && (
            <>
              <div className="text-neutral-400">{t('settings.mesh.supportNumber')}</div>
              <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
                <Typography.Text className="font-mono text-3xl" copyable={!!help.supportId}>
                  {help.supportId?.replace(/^(\d{3})(\d{3})(\d{3})$/, '$1 $2 $3') || '…'}
                </Typography.Text>
                <p className="text-center text-sm text-neutral-400">
                  {t('settings.mesh.supportDesc')}
                </p>
                <span role="status" className={seconds > 0 ? 'text-green-500' : 'text-neutral-400'}>
                  {seconds > 0
                    ? t('settings.mesh.approvalOpen', { time: countdown })
                    : t('settings.mesh.approvalClosed')}
                </span>
                <Button
                  type="primary"
                  loading={deciding}
                  disabled={help.canApprove === false}
                  onClick={() => decide(armSupportApproval)}
                >
                  {seconds > 0
                    ? t('settings.mesh.refreshApproval')
                    : t('settings.mesh.armApproval')}
                </Button>
                <p className="text-center text-xs text-neutral-500">
                  {t('settings.mesh.approvalDesc')}
                </p>
                {help.canApprove === false && (
                  <p className="text-center text-xs text-neutral-400">
                    {t('settings.mesh.approvalOwnerOnly')}
                  </p>
                )}
                {help.authorised && <Tag color="green">{t('settings.mesh.accessApproved')}</Tag>}
                {(help.pending ?? []).map((request) => (
                  <div
                    key={`${request.technician}:${request.sessionId}`}
                    className="w-full space-y-2 rounded border border-neutral-600 p-3"
                  >
                    <strong>{request.agentName || t('settings.mesh.technician')}</strong>
                    <p>{t('settings.mesh.requestDesc')}</p>
                    <p>
                      {t('settings.mesh.verifyCode')} <code>{request.verificationCode}</code>
                    </p>
                    <div className="flex gap-2">
                      <Button
                        type="primary"
                        disabled={deciding || help.canApprove === false}
                        onClick={() =>
                          decide(() =>
                            decideSupportRequest(request.technician, request.sessionId, true)
                          )
                        }
                      >
                        {t('settings.mesh.approveRequest')}
                      </Button>
                      <Button
                        disabled={deciding || help.canApprove === false}
                        onClick={() =>
                          decide(() =>
                            decideSupportRequest(request.technician, request.sessionId, false)
                          )
                        }
                      >
                        {t('settings.mesh.denyRequest')}
                      </Button>
                    </div>
                  </div>
                ))}
              </div>
              <Divider className="opacity-50" />
            </>
          )}

          {/* remote claiming (claim code) — shown only while the device is
              claimable with publicClaims enabled in server.yaml. The policy
              itself is deliberately not settable here: config file only. */}
          {status.claimable && (
            <>
              <div className="text-neutral-400">{t('settings.mesh.remoteClaiming')}</div>
              <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
                {status.publicClaims && status.claimCode ? (
                  <>
                    <Typography.Text className="break-all text-center font-mono text-xl" copyable>
                      {status.claimCode}
                    </Typography.Text>
                    <span className="text-center text-sm text-neutral-400">
                      {t('settings.mesh.claimCodeDesc')}
                    </span>
                    <Button size="small" loading={rotating} onClick={rotateCode}>
                      {t('settings.mesh.rotateCode')}
                    </Button>
                  </>
                ) : (
                  <span className="text-center text-sm text-neutral-400">
                    {t('settings.mesh.remoteClaimingOff')}
                  </span>
                )}
              </div>
              <Divider className="opacity-50" />
            </>
          )}

          {/* status */}
          <div className="text-neutral-400">{t('settings.mesh.status')}</div>
          <div className="mt-5 flex w-full flex-col space-y-5">
            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.claimState')}</span>
              <span>
                {status.claimable
                  ? t('settings.mesh.claimable')
                  : status.fleetName
                    ? t('settings.mesh.claimedFleet', { name: status.fleetName })
                    : t('settings.mesh.claimed')}
              </span>
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.label')}</span>
              <span>{status.label || '-'}</span>
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.attachedTo')}</span>
              {attached ? (
                <span>{attached}</span>
              ) : (
                <span className="text-neutral-500">{t('settings.mesh.notAttached')}</span>
              )}
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.nodeId')}</span>
              {status.nodeId ? (
                <Typography.Text className="break-all font-mono text-xs" copyable>
                  {status.nodeId}
                </Typography.Text>
              ) : (
                <span>-</span>
              )}
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.connection')}</span>
              <span className={status.connected ? 'text-green-500' : 'text-neutral-500'}>
                {status.connected ? t('settings.mesh.connected') : t('settings.mesh.disconnected')}
              </span>
            </div>
          </div>
          <Divider className="opacity-50" />

          {/* memberships */}
          <div className="text-neutral-400">{t('settings.mesh.memberships')}</div>
          <div className="mt-5 flex w-full flex-col space-y-3">
            {status.meshes.length > 0 ? (
              status.meshes.map((mesh) => (
                <div key={mesh.networkId} className="flex w-full items-center justify-between">
                  <span className="break-all font-mono text-sm">{mesh.networkId}</span>
                  <div className="flex shrink-0 items-center pl-2">
                    {mesh.fleet && <Tag color="blue">{t('settings.mesh.fleet')}</Tag>}
                    {mesh.joining && <Tag color="green">{t('settings.mesh.joining')}</Tag>}
                  </div>
                </div>
              ))
            ) : (
              <span className="text-neutral-500">{t('settings.mesh.noMemberships')}</span>
            )}
          </div>

          {/* reset to claim mode — recovery for a device stuck claimed (an
              owner-side unclaim in AllMyStuff that never reached it). The server
              accepts this only from the local network, never over the mesh. */}
          {!status.claimable && (
            <>
              <Divider className="opacity-50" />
              <div className="text-neutral-400">{t('settings.mesh.reset')}</div>
              <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
                <span className="text-center text-sm text-neutral-400">
                  {t('settings.mesh.resetDesc')}
                </span>
                <Popconfirm
                  title={t('settings.mesh.resetConfirmTitle')}
                  description={t('settings.mesh.resetConfirmDesc')}
                  okText={t('settings.mesh.resetConfirmOk')}
                  cancelText={t('settings.mesh.resetConfirmCancel')}
                  okButtonProps={{ danger: true }}
                  onConfirm={resetDevice}
                >
                  <Button danger loading={resetting}>
                    {t('settings.mesh.reset')}
                  </Button>
                </Popconfirm>
              </div>
            </>
          )}
        </>
      )}

      {errMsg && <div className="pt-5 text-red-500">{errMsg}</div>}
    </>
  );
};
