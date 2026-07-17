import { useEffect, useRef, useState } from 'react';
import { Modal, Tooltip } from 'antd';
import clsx from 'clsx';
import { useSetAtom } from 'jotai';
import {
  BadgeInfoIcon,
  CircleArrowUpIcon,
  NetworkIcon,
  PaletteIcon,
  SettingsIcon,
  SmartphoneIcon,
  UserRoundIcon,
  WaypointsIcon
} from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { isKeyboardEnableAtom } from '@/jotai/keyboard.ts';
import { submenuOpenCountAtom } from '@/jotai/settings.ts';
import { Tailscale as TailscaleIcon } from '@/components/icons/tailscale';
import { ScrollArea } from '@/components/ui/scroll-area';

import { About } from './about';
import { Account } from './account';
import { Appearance } from './appearance';
import { Device } from './device';
import { Mesh } from './mesh';
import { Network } from './network';
import { Tailscale } from './tailscale';
import { Update } from './update';

export const Settings = () => {
  const { t } = useTranslation();

  const [isModalOpen, setIsModalOpen] = useState(false);
  const [isLocked, setIsLocked] = useState(false);
  const [currentTab, setCurrentTab] = useState('about');
  const scrollViewportRef = useRef<HTMLDivElement>(null);

  const setIsKeyboardEnable = useSetAtom(isKeyboardEnableAtom);
  const setSubmenuOpenCount = useSetAtom(submenuOpenCountAtom);

  // The Update tab installs OUR firmware from OUR release channel (see
  // ./update and the server's /api/application/update route), never
  // cdn.sipeed.com — a stock update would clobber our mesh server build. The
  // old CDN-polling "check for updates" nag on the gear is gone; the tab does
  // its own check when opened.
  const tabs = [
    { id: 'about', icon: <BadgeInfoIcon size={16} />, component: <About /> },
    { id: 'appearance', icon: <PaletteIcon size={16} />, component: <Appearance /> },
    { id: 'device', icon: <SmartphoneIcon size={16} />, component: <Device /> },
    { id: 'network', icon: <NetworkIcon size={16} />, component: <Network /> },
    { id: 'mesh', icon: <WaypointsIcon size={16} />, component: <Mesh /> },
    {
      id: 'tailscale',
      icon: <TailscaleIcon />,
      component: <Tailscale setIsLocked={setIsLocked} />
    },
    {
      id: 'update',
      icon: <CircleArrowUpIcon size={16} />,
      component: <Update setIsLocked={setIsLocked} />
    },
    { id: 'account', icon: <UserRoundIcon size={18} />, component: <Account /> }
  ];

  useEffect(() => {
    scrollViewportRef.current?.scrollTo({ top: 0, left: 0 });
  }, [currentTab]);

  function changeTab(tab: string) {
    if (isLocked) {
      return;
    }

    setCurrentTab(tab);
  }

  function openModal() {
    setIsModalOpen(true);
    setIsKeyboardEnable(false);
    setSubmenuOpenCount((count) => count + 1);
  }

  function closeModal() {
    if (isLocked) {
      return;
    }

    setIsKeyboardEnable(true);
    setIsModalOpen(false);
    setCurrentTab('about');
    setSubmenuOpenCount((count) => Math.max(0, count - 1));
  }

  return (
    <>
      <Tooltip title={t('settings.title')} placement="bottom" mouseEnterDelay={0.6}>
        <div
          className="flex h-[30px] w-[30px] cursor-pointer items-center justify-center rounded hover:bg-neutral-700/80"
          onClick={openModal}
        >
          <div className="pt-[3px] text-neutral-300 hover:text-white">
            <SettingsIcon size={18} />
          </div>
        </div>
      </Tooltip>

      <Modal
        open={isModalOpen}
        width={'80%'}
        centered={true}
        footer={null}
        destroyOnHidden={true}
        onCancel={closeModal}
        style={{ maxWidth: '1080px' }}
        styles={{ content: { padding: 0 } }}
      >
        <div className="flex h-[80vh] max-h-[700px] rounded-lg outline outline-1 outline-neutral-700">
          <div className="flex h-full max-w-[260px] flex-col space-y-0.5 rounded-l-lg bg-neutral-800/90 px-1 sm:w-1/5 md:w-1/4 md:px-2">
            <div className="hidden px-3 pt-10 text-xl sm:block">{t('settings.title')}</div>
            <div className="h-10 sm:h-5" />
            {tabs.map((tab) => (
              <div
                key={tab.id}
                className={clsx(
                  'flex cursor-pointer select-none items-center space-x-2 rounded-lg p-2 sm:px-3',
                  currentTab === tab.id ? 'bg-neutral-700/50' : 'hover:bg-neutral-700/50'
                )}
                onClick={() => changeTab(tab.id)}
              >
                <div className="h-[16px] w-[16px]">{tab.icon}</div>

                <span className="hidden truncate text-sm sm:block">
                  {t(`settings.${tab.id}.title`)}
                </span>
              </div>
            ))}
          </div>

          <ScrollArea
            viewportRef={scrollViewportRef}
            className="h-full w-full rounded-r-lg bg-neutral-900/50 px-3 [&_[data-slot=scroll-area-scrollbar]]:w-1.5 [&_[data-slot=scroll-area-scrollbar]]:p-0 [&_[data-slot=scroll-area-thumb]]:bg-neutral-500/30"
          >
            <div className="flex h-full w-full justify-center">
              <div className="w-full max-w-[600px] pb-10 pt-14">
                <>{tabs.find((tab) => tab.id === currentTab)?.component}</>
              </div>
            </div>
          </ScrollArea>
        </div>
      </Modal>
    </>
  );
};
