import type { AsyncAcceptedBody } from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export async function sendSupervisorSessionMessage(
  sessionId: string,
  message: string,
): Promise<AsyncAcceptedBody> {
  return supervisorApi().sendSessionMessage(
    activeCityOrThrow('send supervisor session message'),
    sessionId,
    { message },
  );
}
