import { Injectable } from '@angular/core';
import {Observable, of} from 'rxjs';
import {User} from './entities/user';
import {HttpClient, HttpHeaders, HttpParams} from '@angular/common/http';
import {OAuthService} from 'angular-oauth2-oidc';

@Injectable({
  providedIn: 'root'
})
export class UserService {

  private baseUrl = '';

  constructor(private http: HttpClient, private oauthService: OAuthService) {
  }

  getUserInfo(userId: string): Observable<User> {
    /*return this.http.get<User>(this.baseUrl + '/user/detail/get', {
      params: new HttpParams().set('uid', userId)
    });*/
    return of({
      uid: userId,
      firstName: 'userFirstName',
      lastName: 'userLastName',
      email: 'user@email.ch',
    });
  }

  saveUserInfo(userInfo: User): Observable<boolean> {
    // return this.http.post<boolean>(this.baseUrl + '/user/detail/update', userInfo);
    return of(true);
  }

  changeUserPassword(userId: string, newPassword: string): Observable<boolean> {
    // return this.http.post<boolean>(this.baseUrl + '/user/password/change', userInfo);
    return of(true);
  }
}